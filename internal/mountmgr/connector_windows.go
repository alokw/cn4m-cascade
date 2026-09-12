//go:build windows

package mountmgr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsConnector is the Mounter for a native Windows deployment.
//
// Windows has no mount.cifs and needs none: a UNC path is an ordinary path to
// every Win32 file API, so there is nothing to mount and nothing to unmount.
// What a share does need is an authenticated session, and that is what
// WNetAddConnection2 establishes — for the logon session, keyed by the UNC root
// rather than by a mountpoint.
//
// Two consequences, because they are what makes this different rather than
// merely equivalent:
//
//   - Credentials never touch disk. WNetAddConnection2 takes them as arguments
//     in-process, so the 0600 temp file mount.cifs requires — and the window in
//     which it exists — does not happen here at all.
//   - Dir is the UNC root, not a mountpoint. Nothing is created under
//     MOUNT_ROOT, so MOUNT_ROOT is unused on Windows and there is no startup
//     mount hygiene to perform.
type WindowsConnector struct{}

// NewWindowsConnector returns a Mounter backed by the Windows network
// redirector.
func NewWindowsConnector() *WindowsConnector { return &WindowsConnector{} }

// NeedsCredentialsFile reports false: credentials are passed in memory, so the
// manager must not write them anywhere.
func (c *WindowsConnector) NeedsCredentialsFile() bool { return false }

// Flags from winnetwk.h.
const (
	resourceTypeDisk      = 0x00000001
	connectTemporary      = 0x00000004
	cancelConnectionForce = 0x00000001
)

// sep is the Windows path separator, as a string.
const sep = `\`

// netResource mirrors NETRESOURCEW. Only the fields WNetAddConnection2 reads
// for a disk resource are set; the rest must stay nil.
type netResource struct {
	Scope       uint32
	Type        uint32
	DisplayType uint32
	Usage       uint32
	LocalName   *uint16
	RemoteName  *uint16
	Comment     *uint16
	Provider    *uint16
}

var (
	modmpr                     = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2W    = modmpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnection2W = modmpr.NewProc("WNetCancelConnection2W")
	modkernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procGetDiskFreeSpaceExW    = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// uncRoot converts the //host/share form the store produces into the Win32
// form, and discards anything below the share.
func uncRoot(source string) (string, error) {
	s := strings.ReplaceAll(strings.TrimSpace(source), "/", sep)
	s = strings.TrimPrefix(s, sep+sep)
	parts := strings.SplitN(strings.Trim(s, sep), sep, 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("%q is not a host and share path", source)
	}
	return sep + sep + parts[0] + sep + parts[1], nil
}

// Mount authenticates the share. It creates no mountpoint, because Windows has
// none to create.
//
// The call runs on its own goroutine and the caller selects on ctx, for the
// reason every blocking call in this project does: a redirector waiting on an
// unreachable host cannot be interrupted from userspace, so the goroutine is
// abandoned rather than waited for. The channel is buffered so the abandoned
// goroutine still completes and never leaks.
func (c *WindowsConnector) Mount(ctx context.Context, spec MountSpec) error {
	root, err := uncRoot(spec.Source)
	if err != nil {
		return err
	}

	// No password configured means "whatever access this machine already has":
	// a guest or anonymous target. Windows permits only **one** session per
	// server per logon session, so forcing a second one with a different user
	// name fails with ERROR_SESSION_CREDENTIAL_CONFLICT even when the share is
	// already perfectly reachable — which is what happens when somebody has the
	// server open in Explorer. Using the existing session is both what the
	// operator expects and the only thing Windows will allow.
	//
	// A target *with* a password never takes this path: those credentials were
	// configured deliberately, so they are used or the mount fails loudly.
	// Silently borrowing somebody else's session would read and write as the
	// wrong identity.
	if spec.Password == "" && accessible(ctx, root) {
		return nil
	}

	user := spec.Username
	if user != "" && spec.Domain != "" {
		user = spec.Domain + sep + user
	}

	done := make(chan error, 1)
	go func() { done <- addConnection(root, user, spec.Password) }()

	select {
	case err := <-done:
		if err != nil {
			return classifyWindowsMountFailure(spec.Source, err)
		}
		return nil
	case <-ctx.Done():
		return classifyMountFailure(spec.Source, -1, "", ctx.Err())
	}
}

// addConnection is the blocking half of Mount, separate so the syscall is the
// only thing on the abandoned goroutine.
func addConnection(root, user, password string) error {
	remote, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	nr := netResource{Type: resourceTypeDisk, RemoteName: remote}

	var userPtr, passPtr *uint16
	if user != "" {
		if userPtr, err = windows.UTF16PtrFromString(user); err != nil {
			return err
		}
		if passPtr, err = windows.UTF16PtrFromString(password); err != nil {
			return err
		}
	}

	// CONNECT_TEMPORARY: not persisted to the user profile, so credentials set
	// for one run do not silently outlive the process and reconnect at logon.
	ret, _, _ := procWNetAddConnection2W.Call(
		uintptr(unsafe.Pointer(&nr)),
		uintptr(unsafe.Pointer(passPtr)),
		uintptr(unsafe.Pointer(userPtr)),
		uintptr(connectTemporary),
	)
	switch ret {
	case 0, uintptr(windows.ERROR_ALREADY_ASSIGNED), uintptr(windows.ERROR_DEVICE_ALREADY_REMEMBERED):
		// Already connected is success: the share is usable, which is all the
		// caller asked for.
		return nil
	}
	return syscall.Errno(ret)
}

// Unmount drops the session. lazy is ignored: there is no mountpoint to detach,
// and a share whose server has gone must not keep the connection alive, so the
// force flag is always used.
func (c *WindowsConnector) Unmount(ctx context.Context, dir string, _ bool) error {
	root, err := uncRoot(dir)
	if err != nil {
		// Not a UNC path, so nothing was ever connected and nothing to undo.
		return nil
	}

	done := make(chan error, 1)
	go func() {
		name, err := windows.UTF16PtrFromString(root)
		if err != nil {
			done <- err
			return
		}
		ret, _, _ := procWNetCancelConnection2W.Call(
			uintptr(unsafe.Pointer(name)), 0, uintptr(cancelConnectionForce))
		switch ret {
		case 0, uintptr(windows.ERROR_NOT_CONNECTED):
			done <- nil
		default:
			done <- syscall.Errno(ret)
		}
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Mounts returns nothing, and that is correct rather than unimplemented.
//
// The kernel mount table exists so startup hygiene and the stale-mount reaper
// can find mountpoints this process left behind. Windows creates none, so there
// is nothing to find. Returning empty makes both callers no-ops instead of
// errors — which is exactly the warning the first native run produced, reading
// /proc/self/mountinfo on a machine that has no such file.
func (c *WindowsConnector) Mounts() ([]MountInfo, error) { return nil, nil }

// StatFS proves a share still answers, bounded like every call that can land on
// a dead server.
func (c *WindowsConnector) StatFS(ctx context.Context, dir string) (FSStat, error) {
	type result struct {
		stat FSStat
		err  error
	}
	done := make(chan result, 1)

	go func() {
		path, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			done <- result{err: err}
			return
		}
		var availableToCaller, total, free uint64
		ret, _, callErr := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(path)),
			uintptr(unsafe.Pointer(&availableToCaller)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&free)),
		)
		if ret == 0 {
			done <- result{err: fmt.Errorf("reading free space on %s: %w", dir, callErr)}
			return
		}
		// GetDiskFreeSpaceEx reports bytes. A block size of 1 keeps the
		// arithmetic honest rather than inventing a geometry Windows never
		// reported.
		done <- result{stat: FSStat{
			BlockSize:   1,
			Blocks:      total,
			BlocksFree:  free,
			BlocksAvail: availableToCaller,
		}}
	}()

	select {
	case r := <-done:
		return r.stat, r.err
	case <-ctx.Done():
		return FSStat{}, fmt.Errorf("reading free space on %s: %w", dir, ctx.Err())
	}
}

// classifyWindowsMountFailure maps a Win32 error into the same MountError
// vocabulary the Linux path produces, so the API and the UI never learn which
// platform failed.
func classifyWindowsMountFailure(source string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return &MountError{Kind: KindUnknown, Source: source, Detail: err.Error()}
	}

	switch errno {
	case windows.ERROR_SESSION_CREDENTIAL_CONFLICT:
		// Windows allows one session per server per logon session. Another
		// connection to this host already exists under a different user name —
		// very often just an Explorer window — and no credentials can be added
		// alongside it. The remedy is in the message because it is not
		// guessable: nothing about the target is wrong.
		return &MountError{Kind: KindAuth, Source: source, Detail: "Windows already has a connection to this server " +
			"under a different user name, and it allows only one per server. Close any Explorer window on it and run " +
			"`net use " + source + " /delete` (or `net use * /delete` for all of them), or give this target the same " +
			"credentials as the existing connection."}

	case windows.ERROR_LOGON_FAILURE, windows.ERROR_ACCESS_DENIED,
		windows.ERROR_INVALID_PASSWORD, windows.ERROR_NO_SUCH_USER,
		windows.ERROR_ACCOUNT_DISABLED, windows.ERROR_ACCOUNT_RESTRICTION:
		return &MountError{Kind: KindAuth, Source: source, Detail: errno.Error()}
	case windows.ERROR_BAD_NETPATH, windows.ERROR_BAD_NET_NAME:
		return &MountError{Kind: KindShareMissing, Source: source, Detail: errno.Error()}
	case windows.ERROR_NETWORK_UNREACHABLE, windows.ERROR_HOST_UNREACHABLE,
		windows.ERROR_NO_NETWORK, windows.ERROR_NETNAME_DELETED:
		return &MountError{Kind: KindUnreachable, Source: source, Detail: errno.Error()}
	default:
		return &MountError{Kind: KindUnknown, Source: source, Detail: errno.Error()}
	}
}

// accessible reports whether a path can already be listed, bounded like every
// other call that can land on a server that has stopped answering.
//
// Used only to decide whether a session needs establishing at all. A false
// answer is never fatal: the caller goes on to authenticate.
func accessible(ctx context.Context, path string) bool {
	done := make(chan bool, 1)

	go func() {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			done <- false
			return
		}
		attrs, err := windows.GetFileAttributes(name)
		done <- err == nil && attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	}()

	select {
	case ok := <-done:
		return ok
	case <-ctx.Done():
		return false
	}
}
