// Command cn4m-cascade is the sync engine server.
//
// It serves the API and the embedded SPA, runs the sync engine, and fires
// scheduled jobs, and reports to the cn4m suite.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// The IANA zone database, compiled into the binary.
	//
	// Cron schedules are read in the server's local zone (SPEC.md §11, Phase
	// 5a), so a TZ the image cannot resolve means every schedule silently
	// falls back to UTC — the failure the job editor's timezone line exists to
	// make visible. Embedding removes the dependency on a system tzdata
	// package entirely, which costs about 450 KB and works identically on any
	// base image.
	_ "time/tzdata"

	"github.com/alokw/cn4m-cascade/internal/api"
	"github.com/alokw/cn4m-cascade/internal/config"
	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/notify"
	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/scheduler"
	"github.com/alokw/cn4m-cascade/internal/secrets"
	"github.com/alokw/cn4m-cascade/internal/storage"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// shutdownTimeout bounds the whole graceful-shutdown sequence.
const shutdownTimeout = 30 * time.Second

func main() {
	// The container healthcheck, served by the binary itself.
	//
	// The image could shell out to busybox wget, but the binary already knows
	// its own listen address and how to read LISTEN_ADDR, so asking itself is
	// both shorter and immune to the base image changing what it ships.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		// The message is repeated on stderr: a container that dies at
		// startup should say why without needing a log viewer.
		fmt.Fprintf(os.Stderr, "cn4m-cascade: %v\n", err)
		os.Exit(1)
	}
}

// healthcheck probes the local /healthz and returns a process exit code.
//
// Deliberately minimal: it resolves the listen address the same way the server
// does, so a container with a non-default LISTEN_ADDR checks the right port
// rather than silently reporting unhealthy forever.
func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = config.DefaultListenAddr
	}
	// ":2649" is a listen address, not a dial address.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s\n", resp.Status)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	box, err := secrets.NewBox(cfg.EncryptionKey)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("creating the data directory %s: %w", cfg.DataDir, err)
	}
	if err := os.MkdirAll(cfg.MountRoot, 0o755); err != nil {
		return fmt.Errorf("creating the mount root %s: %w", cfg.MountRoot, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("database ready", "path", cfg.DBPath())

	healthc := health.NewCache()

	mounts := mountmgr.New(mountmgr.Config{
		MountRoot:      cfg.MountRoot,
		CredsDir:       filepath.Join(cfg.DataDir, "creds"),
		MountTimeout:   cfg.MountTimeout,
		StatFSTimeout:  cfg.StatFSTimeout,
		UnmountTimeout: cfg.UnmountTimeout,
		IdleGrace:      cfg.IdleGrace,
		Params:         mountmgr.Params{UID: cfg.MountUID, GID: cfg.MountGID},
	}, mountmgr.NewExecMounter(), db, decryptPassword(box), healthc, log)

	// SPEC.md §5, startup hygiene: clear anything an unclean shutdown left
	// behind before serving traffic.
	if err := mounts.ReconcileStale(ctx); err != nil {
		log.Warn("startup mount cleanup did not complete", "error", err)
	}
	mounts.Start()

	// A run that was in progress when the process died will never finish;
	// mark it failed rather than leaving it "running" forever.
	if n, err := db.ReconcileInterruptedRuns(ctx); err != nil {
		log.Warn("could not reconcile interrupted runs", "error", err)
	} else if n > 0 {
		log.Warn("marked interrupted runs as failed", "count", n)
	}

	provider := storage.NewProvider(mounts, healthc)
	runs := runner.New(db, provider, log)

	// Outbound callbacks (SPEC.md §8.2). The notifier is given a way to read a
	// webhook's signing key rather than the encryption key itself, exactly as
	// the mount manager is given a way to read a target's password.
	// The suite-reporting callback, created once on a fresh installation at
	// whatever CN4M_CASCADE_STATUS_URL says. "off" reports nowhere — for a cascade
	// that is not part of a cn4m suite, or a developer who does not want a
	// local run showing up in a shared status view.
	if cfg.CN4MStatusURL == config.CN4MReportingOff {
		log.Info("cn4m suite reporting is off (CN4M_CASCADE_STATUS_URL=off)")
	} else if created, err := db.EnsureCN4MWebhook(ctx, cfg.CN4MStatusURL); err != nil {
		// Not fatal: failing to set up a status callback is no reason to
		// refuse to run backups.
		log.Warn("could not create the cn4m status callback", "error", err)
	} else if created {
		log.Info("created the cn4m status callback", "url", cfg.CN4MStatusURL)
	}

	notifier := notify.New(db, box.Decrypt, log)
	// Its OWN context, deliberately not the signal context. On SIGTERM `ctx`
	// is cancelled immediately, which would stop every delivery worker before
	// the runs being cancelled below had produced their run_completed events —
	// so the last thing an integration hears would be "started", and the
	// callbacks would be dropped into a channel nobody is reading. The
	// notifier is instead stopped explicitly, last, once the runs that feed it
	// have finished.
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	defer stopNotify()
	notifier.Start(notifyCtx)
	runs.SetNotifier(notifier)

	server := api.NewServer(db, provider, mounts, healthc, box, runs, log)

	// CN4M_CASCADE_ADMIN_PASSWORD seeds first-run setup so a container can come up
	// already configured (SPEC.md §8). It never overwrites an existing
	// password: an env var left in a compose file must not silently reset
	// the credential every restart.
	//
	// An *empty* value means "not configured", not "no password" — even though
	// a blank password is otherwise a supported choice. SPEC.md §10's compose
	// file passes `ADMIN_PASSWORD=${ADMIN_PASSWORD}`, which expands to the
	// empty string when the variable is unset on the host, so treating empty
	// as a deliberate blank would turn a forgotten variable into a server
	// anyone can sign into. Choosing no password has to be an explicit act, so
	// it is only available through the first-run setup form.
	if pw := os.Getenv("CN4M_CASCADE_ADMIN_PASSWORD"); pw != "" {
		set, err := db.AdminPasswordSet(ctx)
		if err != nil {
			return fmt.Errorf("checking whether an admin password is set: %w", err)
		}
		if !set {
			if err := db.SetAdminPassword(ctx, pw); err != nil {
				return fmt.Errorf("setting the admin password from CN4M_CASCADE_ADMIN_PASSWORD: %w", err)
			}
			log.Info("admin password set from CN4M_CASCADE_ADMIN_PASSWORD")
		}
	}

	server.Start(ctx)

	// Started after the interrupted-run reconciliation above, so a job whose
	// last run died with the process is not considered still-running by the
	// overlap check the moment a schedule fires.
	sched := scheduler.New(db, runs, log)
	sched.Start(ctx)
	log.Info("scheduler started", "tick", scheduler.TickInterval, "timezone", time.Local.String())

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// SPEC.md §10: flush, then lazy-unmount every share.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http server did not shut down cleanly", "error", err)
	}
	// Disconnect event-feed clients before the runs they are watching go
	// away, so nothing is broadcasting into a closing database.
	server.Stop()
	// Stopped before runs are cancelled, so a tick cannot start a new run
	// into a server that is on its way down.
	if err := sched.Shutdown(shutdownCtx); err != nil {
		log.Warn("scheduler did not stop cleanly", "error", err)
	}
	// SPEC.md §10: cancel running jobs, marking them cancelled, before the
	// mounts they are using go away.
	if err := runs.Shutdown(shutdownCtx); err != nil {
		log.Warn("runs did not stop cleanly", "error", err)
	}
	if err := mounts.Shutdown(shutdownCtx); err != nil {
		log.Warn("mount cleanup did not complete", "error", err)
	}
	// Last, and after the runs that produce them: a "run_completed" callback
	// for the run that just stopped is worth the moment it takes to send.
	if err := notifier.Stop(shutdownCtx); err != nil {
		log.Warn("callback delivery did not drain", "error", err)
	}
	stopNotify()
	log.Info("stopped")
	return nil
}

// decryptPassword gives the mount manager a way to obtain a plaintext
// password without ever holding the encryption key itself.
func decryptPassword(box *secrets.Box) mountmgr.CredentialLookup {
	return func(_ context.Context, t *store.Target) (string, error) {
		return box.Decrypt(t.PasswordEncrypted)
	}
}

func logLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
