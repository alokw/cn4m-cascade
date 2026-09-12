//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// runService runs in the foreground, which is the only mode off Windows.
//
// Linux deployment is a container with an init that sends SIGTERM (SPEC.md
// §10), so there is no service manager to integrate with — the signal handler
// *is* the integration.
func runService(log *slog.Logger) error {
	return runForeground(log)
}

// runForeground blocks until SIGINT or SIGTERM.
func runForeground(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx, log)
}

// serviceCommand reports whether os.Args asks for a service subcommand. Always
// false here: there is no service manager to talk to.
func serviceCommand() (string, bool) { return "", false }

// runServiceCommand should be unreachable; serviceCommand never reports one.
func runServiceCommand(string) error { panic("service subcommands are Windows-only") }

var _ = os.Args
