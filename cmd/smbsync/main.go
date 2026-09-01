// Command smbsync is the sync engine server.
//
// Phase 1 (SPEC.md §11) serves target CRUD and connection testing; the sync
// engine, scheduler and UI arrive in later phases.
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
	"syscall"
	"time"

	"github.com/alokw/cn4m-cascade/internal/api"
	"github.com/alokw/cn4m-cascade/internal/config"
	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/mountmgr"
	"github.com/alokw/cn4m-cascade/internal/runner"
	"github.com/alokw/cn4m-cascade/internal/secrets"
	"github.com/alokw/cn4m-cascade/internal/storage"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// shutdownTimeout bounds the whole graceful-shutdown sequence.
const shutdownTimeout = 30 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		// The message is repeated on stderr: a container that dies at
		// startup should say why without needing a log viewer.
		fmt.Fprintf(os.Stderr, "smbsync: %v\n", err)
		os.Exit(1)
	}
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
	server := api.NewServer(db, provider, mounts, healthc, box, runs, log)

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
	// SPEC.md §10: cancel running jobs, marking them cancelled, before the
	// mounts they are using go away.
	if err := runs.Shutdown(shutdownCtx); err != nil {
		log.Warn("runs did not stop cleanly", "error", err)
	}
	if err := mounts.Shutdown(shutdownCtx); err != nil {
		log.Warn("mount cleanup did not complete", "error", err)
	}
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
