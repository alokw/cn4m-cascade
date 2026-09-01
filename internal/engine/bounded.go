package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// DefaultOpTimeout bounds a single filesystem operation against a share.
//
// It is generous on purpose: it is a backstop against a wedged server, not a
// performance limit. A metadata call that takes longer than this on a healthy
// share indicates something is badly wrong.
const DefaultOpTimeout = 30 * time.Second

// bounded runs fn on its own goroutine and gives up waiting when ctx expires.
//
// This is the pattern CLAUDE.md requires for every blocking call against an
// SMB path, and the reason it is needed is that a syscall stuck in the kernel
// cannot be interrupted from userspace: os.ReadDir on a share whose server
// has vanished simply does not return. What this bounds is how long the
// *caller* waits, which is what makes cancellation and shutdown responsive.
//
// The abandoned goroutine is not free — it holds an OS thread until the
// kernel finally releases it. That is unavoidable and is the price of not
// hanging; the alternative is a process that never exits. The channel is
// buffered so the goroutine always completes and never leaks beyond the
// syscall itself.
func bounded[T any](ctx context.Context, timeout time.Duration, what string, fn func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}

	if timeout <= 0 {
		timeout = DefaultOpTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ch := make(chan result, 1)
	go func() {
		value, err := fn()
		ch <- result{value: value, err: err}
	}()

	select {
	case r := <-ch:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("%s did not respond in time: %w", what, ctx.Err())
	}
}

// boundedVoid is bounded for operations with no return value.
func boundedVoid(ctx context.Context, timeout time.Duration, what string, fn func() error) error {
	_, err := bounded(ctx, timeout, what, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

func boundedReadDir(ctx context.Context, timeout time.Duration, path string) ([]os.DirEntry, error) {
	return bounded(ctx, timeout, "listing "+path, func() ([]os.DirEntry, error) {
		return os.ReadDir(path)
	})
}

func boundedInfo(ctx context.Context, timeout time.Duration, entry os.DirEntry, path string) (fs.FileInfo, error) {
	return bounded(ctx, timeout, "reading the details of "+path, entry.Info)
}

func boundedOpen(ctx context.Context, timeout time.Duration, path string) (*os.File, error) {
	return bounded(ctx, timeout, "opening "+path, func() (*os.File, error) {
		return os.Open(path)
	})
}

func boundedMkdirAll(ctx context.Context, timeout time.Duration, path string) error {
	return boundedVoid(ctx, timeout, "creating "+path, func() error {
		return os.MkdirAll(path, 0o755)
	})
}

func boundedRemove(ctx context.Context, timeout time.Duration, path string) error {
	return boundedVoid(ctx, timeout, "removing "+path, func() error {
		return os.Remove(path)
	})
}

// ReadFileBounded reads a whole file without letting a dead share trap the
// caller. Exported because filter rule files may live on a target
// (SPEC.md §6.5's target:// references), and those are share paths like any
// other.
func ReadFileBounded(ctx context.Context, timeout time.Duration, path string) ([]byte, error) {
	return bounded(ctx, timeout, "reading "+path, func() ([]byte, error) {
		return os.ReadFile(path)
	})
}

func boundedRename(ctx context.Context, timeout time.Duration, from, to string) error {
	return boundedVoid(ctx, timeout, "renaming into place at "+to, func() error {
		return os.Rename(from, to)
	})
}
