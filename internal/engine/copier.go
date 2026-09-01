package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultStallTimeout is how long a copy may make no progress at all before
// it is abandoned.
const DefaultStallTimeout = 30 * time.Second

// ErrStalled means a copy stopped moving bytes entirely. It is treated as a
// transport failure, because that is overwhelmingly what causes it.
var ErrStalled = errors.New("the copy stopped making progress")

// ErrShareUnreachable means a copy failed in a way that indicates the share
// itself has gone away, rather than a problem with one file. Retrying every
// remaining file against a dead server would take approximately forever, so
// this aborts the run (SPEC.md §5).
var ErrShareUnreachable = errors.New("the share stopped responding")

// ErrSourceVanished means the file disappeared between the scan and the copy.
// It is expected on a live tree and is reported as a warning, not a failure
// (SPEC.md §12).
var ErrSourceVanished = errors.New("the file no longer exists at the source")

// Copy buffer sizing. SPEC.md §6.3 calls for 1–4 MiB explicitly: the default
// 32 KiB of io.Copy leaves a gigabit link idle between round trips.
const (
	DefaultBufferSize = 4 << 20
	minBufferSize     = 64 << 10
)

// DefaultRetryBackoff is SPEC.md §6.3's policy: three attempts in total,
// with these delays between them.
var DefaultRetryBackoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}

// attemptsFor caps the total number of tries at what SPEC.md §6.3 asks for:
// three. A longer backoff list only lengthens the waits, never the count.
func attemptsFor(backoff []time.Duration) int {
	const specAttempts = 3
	if len(backoff)+1 < specAttempts {
		return len(backoff) + 1
	}
	return specAttempts
}

// Copier copies one file at a time, durably.
//
// Every copy goes to a temp file in the destination directory, is flushed,
// and is then renamed over the target name, so an interrupted copy can never
// leave a half-written file where a good one used to be (CLAUDE.md).
type Copier struct {
	// BufferSize is the userspace copy buffer. Zero means DefaultBufferSize.
	BufferSize int
	// Backoff holds the delay before each retry. Zero means
	// DefaultRetryBackoff. len(Backoff)+1 is the number of attempts.
	Backoff []time.Duration

	// ShouldRetry, if set, is consulted before each retry of an otherwise
	// retryable error. Returning false gives up immediately. The executor
	// uses it to stop retrying once the share has been confirmed dead,
	// rather than spending the full backoff budget on every queued file.
	// It may be called from several goroutines at once.
	ShouldRetry func(err error) bool

	// StallTimeout abandons an attempt when no bytes have moved for this
	// long. Zero means DefaultStallTimeout.
	//
	// A total timeout would be wrong — a legitimate multi-gigabyte file may
	// take many minutes — so what is bounded is *inactivity*, which is what
	// a dead server actually looks like.
	StallTimeout time.Duration

	// OpTimeout bounds the individual metadata calls around a copy: open,
	// sync, chtimes, rename. Zero means DefaultOpTimeout.
	OpTimeout time.Duration

	pool     sync.Pool
	poolOnce sync.Once
}

// CopyResult reports what one file copy did.
type CopyResult struct {
	BytesCopied int64
	Attempts    int
}

// Copy writes src to dst, preserving the modification time.
//
// onBytes, if set, is called with the size of each chunk as it is written, so
// a progress tracker can follow a large file in flight. It may be called from
// this goroutine only.
func (c *Copier) Copy(ctx context.Context, src, dst string, modTime time.Time, onBytes func(n int64)) (CopyResult, error) {
	backoff := c.Backoff
	if backoff == nil {
		backoff = DefaultRetryBackoff
	}

	var result CopyResult
	var lastErr error

	// discard rewinds bytes an abandoned attempt had already reported, so a
	// failed copy never leaves phantom progress behind. Every exit path
	// below that is not a success must go through it, or bytesDone drifts
	// above bytesTotal and the ETA collapses to zero while work remains.
	discard := func(written int64) {
		if onBytes != nil && written > 0 {
			onBytes(-written)
		}
	}

	for attempt := 0; attempt < attemptsFor(backoff); attempt++ {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("copying %s was cancelled: %w", src, err)
		}

		result.Attempts = attempt + 1
		written, err := c.copyOnce(ctx, src, dst, modTime, onBytes)
		if err == nil {
			result.BytesCopied = written
			return result, nil
		}
		lastErr = err

		discard(written)

		if !retryable(err) {
			return result, err
		}
		if c.ShouldRetry != nil && !c.ShouldRetry(err) {
			return result, fmt.Errorf("%w: %w", ErrShareUnreachable, err)
		}
		if attempt == attemptsFor(backoff)-1 {
			break
		}

		select {
		case <-time.After(backoff[attempt]):
		case <-ctx.Done():
			return result, fmt.Errorf("copying %s was cancelled: %w", src, ctx.Err())
		}
	}

	// Exhausting every attempt against a transport failure means the share
	// is gone, not that this one file is troublesome.
	if TransportError(lastErr) {
		return result, fmt.Errorf("%w after %d attempts: %w", ErrShareUnreachable, result.Attempts, lastErr)
	}
	return result, fmt.Errorf("gave up after %d attempts: %w", result.Attempts, lastErr)
}

// copyOnce runs one attempt with an inactivity watchdog.
//
// The work happens on its own goroutine so that a read or a flush wedged in
// the kernel cannot trap the caller: if nothing moves for StallTimeout, or
// the context is cancelled, this returns and leaves the goroutine to finish
// on its own. That goroutine still owns its temp file and removes it when it
// eventually completes, so an abandoned attempt never leaves debris behind.
func (c *Copier) copyOnce(ctx context.Context, src, dst string, modTime time.Time, onBytes func(n int64)) (int64, error) {
	type outcome struct {
		written int64
		err     error
	}

	stallTimeout := c.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = DefaultStallTimeout
	}

	done := make(chan outcome, 1)
	// Heartbeats only need to be noticed, not counted, so a full buffer is
	// harmless and the send never blocks the copy.
	beat := make(chan struct{}, 1)

	var progressed atomic.Int64
	go func() {
		written, err := c.copyOnceBlocking(ctx, src, dst, modTime, func(n int64) {
			progressed.Add(n)
			if onBytes != nil {
				onBytes(n)
			}
			select {
			case beat <- struct{}{}:
			default:
			}
		})
		done <- outcome{written: written, err: err}
	}()

	timer := time.NewTimer(stallTimeout)
	defer timer.Stop()

	for {
		select {
		case res := <-done:
			return res.written, res.err

		case <-beat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(stallTimeout)

		case <-timer.C:
			return progressed.Load(), fmt.Errorf("copying %s: %w after %v", src, ErrStalled, stallTimeout)

		case <-ctx.Done():
			return progressed.Load(), ctx.Err()
		}
	}
}

// copyOnceBlocking is the actual attempt: temp file, copy, flush, rename,
// set mtime. Every step is bounded so it cannot wedge indefinitely.
func (c *Copier) copyOnceBlocking(ctx context.Context, src, dst string, modTime time.Time, onBytes func(n int64)) (int64, error) {
	in, err := boundedOpen(ctx, c.OpTimeout, src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%s: %w", src, ErrSourceVanished)
		}
		return 0, fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	tmpPath, out, err := createTemp(dst)
	if err != nil {
		return 0, err
	}
	// Any failure past this point must not leave the temp file behind.
	committed := false
	defer func() {
		out.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	buf := c.buffer()
	defer c.pool.Put(buf)

	written, err := copyBuffered(ctx, out, in, *buf, onBytes)
	if err != nil {
		return written, err
	}

	// Flush to the server before the rename, so the rename can never publish
	// a name that points at incomplete data.
	if err := boundedVoid(ctx, c.OpTimeout, "flushing "+dst, out.Sync); err != nil {
		return written, err
	}
	if err := out.Close(); err != nil {
		return written, fmt.Errorf("closing %s: %w", dst, err)
	}

	// The mtime is set on the temp file, before the rename, so the file is
	// never visible at its final name with the wrong timestamp. Preserving
	// it is what makes a re-run a no-op (CLAUDE.md).
	if !modTime.IsZero() {
		err := boundedVoid(ctx, c.OpTimeout, "preserving the modification time of "+dst, func() error {
			return os.Chtimes(tmpPath, modTime, modTime)
		})
		if err != nil {
			return written, err
		}
	}

	if err := boundedRename(ctx, c.OpTimeout, tmpPath, dst); err != nil {
		return written, err
	}
	committed = true
	return written, nil
}

// copyBuffered is io.CopyBuffer with cancellation between chunks and a
// progress callback.
func copyBuffered(ctx context.Context, dst io.Writer, src io.Reader, buf []byte, onBytes func(n int64)) (int64, error) {
	var written int64

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			written += int64(nw)
			if onBytes != nil && nw > 0 {
				onBytes(int64(nw))
			}
			if writeErr != nil {
				return written, writeErr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

// tempWrapper is the fixed part of a temp filename: a leading dot, two
// separating dots, 16 hex characters and the extension.
const tempWrapper = ".." + "0123456789abcdef" + ".tmp"

// createTemp opens the temp file that a copy is staged through. It lives in
// the destination directory so the final rename is same-filesystem and
// therefore atomic.
func createTemp(dst string) (string, *os.File, error) {
	dir := filepath.Dir(dst)

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, fmt.Errorf("naming a temporary file: %w", err)
	}
	// The wrapper adds 22 characters. A source name within 22 of NAME_MAX
	// would otherwise fail with ENAMETOOLONG, and SPEC.md §12 explicitly
	// calls for very long paths to work.
	const nameMax = 255
	base := filepath.Base(dst)
	if overflow := len(base) + len(tempWrapper); overflow > nameMax {
		base = base[:len(base)-(overflow-nameMax)]
	}
	name := fmt.Sprintf(".%s.%s.tmp", base, hex.EncodeToString(suffix))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", nil, fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	return path, f, nil
}

func (c *Copier) buffer() *[]byte {
	c.poolOnce.Do(func() {
		size := c.BufferSize
		if size <= 0 {
			size = DefaultBufferSize
		}
		if size < minBufferSize {
			size = minBufferSize
		}
		c.pool.New = func() any {
			b := make([]byte, size)
			return &b
		}
	})
	return c.pool.Get().(*[]byte)
}

// TransportError reports whether an error looks like the connection to the
// server failing, as opposed to something about this particular file.
func TransportError(err error) bool {
	if errors.Is(err, ErrStalled) {
		return true
	}
	for _, errno := range []syscall.Errno{
		syscall.EIO, syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ETIMEDOUT,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED,
		syscall.ENOTCONN, syscall.ENETDOWN, syscall.ENETUNREACH,
		syscall.EPIPE, syscall.ESTALE, syscall.ENODEV,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// retryable decides whether another attempt could plausibly succeed
// (SPEC.md §6.3). Permission problems and a full disk will fail identically
// every time; I/O errors against a flaky share are exactly what retries are
// for.
func retryable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrSourceVanished), errors.Is(err, fs.ErrNotExist):
		return false
	case errors.Is(err, fs.ErrPermission):
		return false
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return false
	case errors.Is(err, syscall.EROFS):
		return false
	default:
		return true
	}
}
