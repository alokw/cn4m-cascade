//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alokw/cn4m-cascade/internal/config"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is what the SCM knows this by. Stable across releases: renaming
// it would orphan an installed service rather than upgrade it.
const serviceName = "cn4m-cascade"

const serviceDisplayName = "cn4m cascade sync engine"

const serviceDescription = "Scheduled SMB synchronisation with a local web UI on port 2649."

// stopWaitHint is what the SCM is told to expect while stopping.
//
// SPEC.md §10 budgets 30 seconds for graceful shutdown, because lazily
// detaching a share whose server has gone is the slow case. The SCM kills a
// service that stops taking longer than its hint, so this must exceed that
// budget or Windows would terminate the process in exactly the situation the
// budget exists for.
const stopWaitHint = 45 * time.Second

// runService picks how to run: under the service control manager if Windows
// started us that way, otherwise in the foreground.
//
// svc.IsWindowsService is the reliable test. Guessing from the absence of a
// console, or from a flag the operator has to remember, both get this wrong in
// the case that matters — a service that starts, decides it is interactive, and
// is killed by the SCM for never reporting Running.
func runService(log *slog.Logger) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("determining whether this is a service: %w", err)
	}
	if !isService {
		return runForeground(log)
	}
	return svc.Run(serviceName, &cascadeService{log: log})
}

// runForeground blocks until Ctrl+C or a termination signal.
func runForeground(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx, log)
}

// cascadeService adapts run() to the service control manager.
type cascadeService struct {
	log *slog.Logger
}

// Execute is the SCM's entry point.
//
// The shape matters: report StartPending, get the work running, report Running,
// then translate Stop and Shutdown into cancelling the context that run() is
// already built around. Nothing about shutdown is special-cased here — the same
// graceful path the container's SIGTERM takes.
func (s *cascadeService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending, WaitHint: uint32(stopWaitHint / time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	elog, elogErr := eventlog.Open(serviceName)
	if elogErr == nil {
		defer elog.Close()
	}
	// Event Log is where a Windows operator looks first, and a service with no
	// console has nowhere else to say "I stopped and why". slog still writes
	// its JSON; this is in addition, not instead.
	info := func(msg string) {
		if elog != nil {
			_ = elog.Info(1, msg)
		}
	}
	failed := func(msg string) {
		if elog != nil {
			_ = elog.Error(1, msg)
		}
	}

	done := make(chan error, 1)
	go func() { done <- run(ctx, s.log) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	info(serviceDisplayName + " started")

	for {
		select {
		case err := <-done:
			// Exited on its own, which for a service means something broke:
			// nothing asked it to stop.
			if err != nil {
				s.log.Error("service exited", "error", err)
				failed(serviceDisplayName + " exited with an error: " + err.Error())
				changes <- svc.Status{State: svc.Stopped}
				// A non-zero exit code is what makes the SCM's recovery
				// settings ("restart on failure") apply.
				return false, 1
			}
			changes <- svc.Status{State: svc.Stopped}
			return false, 0

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus

			case svc.Stop, svc.Shutdown:
				// Tell the SCM to wait: the graceful path can legitimately
				// take tens of seconds when a share has gone away.
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopWaitHint / time.Millisecond)}
				info(serviceDisplayName + " stopping")
				cancel()

				select {
				case err := <-done:
					if err != nil {
						failed(serviceDisplayName + " stopped with an error: " + err.Error())
						changes <- svc.Status{State: svc.Stopped}
						return false, 1
					}
				case <-time.After(stopWaitHint):
					// The bound exists for the same reason every other one in
					// this project does: something wedged in a syscall cannot
					// be waited out, and the SCM will kill us anyway.
					failed(serviceDisplayName + " did not finish stopping within " + stopWaitHint.String())
				}
				changes <- svc.Status{State: svc.Stopped}
				return false, 0

			default:
				s.log.Warn("unexpected service control request", "cmd", c.Cmd)
			}
		}
	}
}

// serviceCommand reports a `-service <cmd>` request, if present.
func serviceCommand() (string, bool) {
	if len(os.Args) < 3 || os.Args[1] != "-service" {
		return "", false
	}
	return strings.ToLower(os.Args[2]), true
}

// runServiceCommand performs install, uninstall, start or stop.
//
// The checks that do not need administrator rights run first, deliberately. An
// operator who mistyped the subcommand, or who has not set ENCRYPTION_KEY,
// should be told that — not "Access is denied" from a service manager they were
// never going to reach.
func runServiceCommand(cmd string) error {
	switch cmd {
	case "install", "uninstall", "start", "stop":
	default:
		return fmt.Errorf("unknown service command %q: expected install, uninstall, start or stop", cmd)
	}

	if cmd == "install" {
		if err := checkServiceEncryptionKey(); err != nil {
			return err
		}
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service manager (this needs an elevated prompt): %w", err)
	}
	defer m.Disconnect()

	switch cmd {
	case "install":
		return installService(m)
	case "uninstall":
		return uninstallService(m)
	default:
		return controlService(m, cmd)
	}
}

func installService(m *mgr.Mgr) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding this executable: %w", err)
	}

	if existing, err := m.OpenService(serviceName); err == nil {
		existing.Close()
		return fmt.Errorf("%s is already installed; uninstall it first", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("creating the service: %w", err)
	}
	defer s.Close()

	// Registering the event source is what stops the Event Log showing
	// "the description for Event ID 1 cannot be found".
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Not fatal: the service works, its log entries are just uglier.
		fmt.Fprintf(os.Stderr, "warning: could not register the event source: %v\n", err)
	}

	fmt.Printf("installed %s (%s), start type automatic\n", serviceName, exe)
	fmt.Printf("start it with:  %s -service start\n", exe)
	return nil
}

func uninstallService(m *mgr.Mgr) error {
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("opening %s (is it installed?): %w", serviceName, err)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("deleting the service: %w", err)
	}
	if err := eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the event source: %v\n", err)
	}
	fmt.Printf("uninstalled %s\n", serviceName)
	fmt.Println("the database and ENCRYPTION_KEY are untouched; removing the service does not remove your data")
	return nil
}

func controlService(m *mgr.Mgr, action string) error {
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("opening %s (is it installed?): %w", serviceName, err)
	}
	defer s.Close()

	if action == "start" {
		if err := s.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", serviceName, err)
		}
		fmt.Printf("started %s\n", serviceName)
		return nil
	}

	status, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("stopping %s: %w", serviceName, err)
	}
	// Wait for it, with the same budget the service asks the SCM for, so
	// `-service stop` returning means it is actually stopped.
	deadline := time.Now().Add(stopWaitHint)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not stop within %v", serviceName, stopWaitHint)
		}
		time.Sleep(500 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			return fmt.Errorf("querying %s: %w", serviceName, err)
		}
	}
	fmt.Printf("stopped %s\n", serviceName)
	return nil
}

// checkServiceEncryptionKey verifies the service will find an ENCRYPTION_KEY
// when it starts, from either of the two places it can come from.
//
// A service does **not** inherit the environment of the prompt that installed
// it, so os.Getenv here would happily find a key set only in this shell and
// approve an install that then refuses to start. The two sources that do reach a
// service are:
//
//  1. a config file beside the executable, which the process reads itself; and
//  2. the machine environment.
//
// Either is sufficient. Requiring the machine environment when a `.env` is
// already sitting next to the binary would be a check that fails on a
// deployment that works.
func checkServiceEncryptionKey() error {
	if fileSetsEncryptionKey() || machineEncryptionKeySet() {
		return nil
	}
	// A raw literal: the message is several lines of instructions, and the
	// operator reads it at a prompt.
	return errors.New(`the service will not find an ENCRYPTION_KEY.
Set it in one of the two places a service can read:
  1. a .env file beside the executable, containing:
         ENCRYPTION_KEY=<a long random value>
  2. or the machine environment, from an elevated prompt:
         setx /M ENCRYPTION_KEY "<a long random value>"
A service does not inherit the environment of the prompt that installs it, which
is why setting it for this shell alone is not enough.`)
}

// fileSetsEncryptionKey reports whether the config file the process will
// discover sets a non-empty ENCRYPTION_KEY.
func fileSetsEncryptionKey() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	path := filepath.Join(filepath.Dir(exe), ".env")
	if explicit := strings.TrimSpace(os.Getenv(config.ConfigPathVar)); explicit != "" {
		path = explicit
	}
	values, err := config.LoadEnvFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(values["ENCRYPTION_KEY"]) != ""
}

// machineEncryptionKeySet reads the machine environment from the registry.
//
// The registry rather than os.Getenv, deliberately: os.Getenv cannot tell a
// machine-wide value from one set in this shell, and that distinction is the
// entire point of the check.
func machineEncryptionKeySet() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()

	value, _, err := k.GetStringValue("ENCRYPTION_KEY")
	return err == nil && strings.TrimSpace(value) != ""
}
