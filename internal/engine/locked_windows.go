//go:build windows

package engine

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// lockProbeTimeout bounds the probe below. It is short because the probe is a
// diagnostic on a path that has already failed: a better error message is not
// worth making a failing run wait.
const lockProbeTimeout = 5 * time.Second

// lockedByAnotherProcess reports whether an error unambiguously means a file is
// held open by somebody else.
//
// Windows-only. Linux replaces a file another process has open without
// complaint — the old inode lives until the last handle closes — so the whole
// class of failure does not arise there.
func lockedByAnotherProcess(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_USER_MAPPED_FILE)
}

// renameBlockedByLock reports whether a failed rename failed because something
// holds the destination open.
//
// **The unambiguous codes are not enough, which live testing is how we found
// out.** Renaming over a file held with FILE_SHARE_NONE on an SMB share fails
// with plain ERROR_ACCESS_DENIED, not ERROR_SHARING_VIOLATION — the redirector
// reports the refusal, not its cause. Treating every ACCESS_DENIED as a lock
// would be worse than the problem: a genuinely read-only destination, or an
// account without delete rights, would then be reported as "another process has
// it open" and send the operator hunting for an application that does not
// exist.
//
// So ACCESS_DENIED is disambiguated by asking the question directly: can this
// process open the destination for DELETE while allowing every kind of sharing?
// If that comes back ERROR_SHARING_VIOLATION, somebody else has it open without
// sharing delete, and the rename could never have succeeded. If it opens, the
// rights are there and ACCESS_DENIED meant something else, which is left to
// report itself.
//
// The probe opens for DELETE access; it does not delete. Nothing is written and
// the handle is closed immediately.
func renameBlockedByLock(err error, dst string) bool {
	if lockedByAnotherProcess(err) {
		return true
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false
	}
	return heldOpenElsewhere(dst)
}

// heldOpenElsewhere probes a path, bounded, because it is a file open on what
// may be a share whose server has stopped answering. A probe that cannot
// complete reports false: the caller then keeps the original error, which is
// the honest outcome when we could not establish the cause.
func heldOpenElsewhere(dst string) bool {
	done := make(chan bool, 1)

	go func() {
		name, err := windows.UTF16PtrFromString(dst)
		if err != nil {
			done <- false
			return
		}
		h, err := windows.CreateFile(
			name,
			windows.DELETE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err != nil {
			// A sharing violation here is the positive answer: the file is open
			// elsewhere in a mode that excludes what the rename needed.
			done <- errors.Is(err, windows.ERROR_SHARING_VIOLATION)
			return
		}
		windows.CloseHandle(h)
		done <- false
	}()

	select {
	case locked := <-done:
		return locked
	case <-time.After(lockProbeTimeout):
		return false
	}
}
