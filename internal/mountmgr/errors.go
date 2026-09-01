package mountmgr

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FailureKind classifies a mount failure. It drives both the user-facing
// message and whether retrying at a lower SMB dialect could possibly help.
type FailureKind string

const (
	KindAuth         FailureKind = "auth"          // bad username/password/domain
	KindShareMissing FailureKind = "share_missing" // host reachable, share is not there
	KindUnreachable  FailureKind = "unreachable"   // host down, refused, no route, DNS
	KindTimeout      FailureKind = "timeout"       // our own deadline expired
	KindDialect      FailureKind = "dialect"       // server rejected the SMB version or an option
	KindPermission   FailureKind = "permission"    // we are not allowed to mount at all
	KindUnknown      FailureKind = "unknown"
)

// MountError is a mount.cifs failure translated into something a user can
// act on. Detail keeps the raw output for the API's detail field.
type MountError struct {
	Source   string
	Kind     FailureKind
	Errno    int
	ExitCode int
	Detail   string
}

func (e *MountError) Error() string {
	switch e.Kind {
	case KindAuth:
		return fmt.Sprintf("authentication failed for %s — check the username, password and domain", e.Source)
	case KindShareMissing:
		return fmt.Sprintf("share %s does not exist on the server, or the account cannot see it", e.Source)
	case KindUnreachable:
		return fmt.Sprintf("cannot reach %s — the host is down, or SMB (445/tcp) is blocked", e.Source)
	case KindTimeout:
		return fmt.Sprintf("mounting %s timed out", e.Source)
	case KindDialect:
		return fmt.Sprintf("the server rejected the connection settings for %s (SMB version or mount options not supported)", e.Source)
	case KindPermission:
		return fmt.Sprintf("not permitted to mount %s — the container needs the SYS_ADMIN capability", e.Source)
	default:
		if e.Detail != "" {
			return fmt.Sprintf("mounting %s failed: %s", e.Source, firstLine(e.Detail))
		}
		return fmt.Sprintf("mounting %s failed (exit status %d)", e.Source, e.ExitCode)
	}
}

// DialectRetryable reports whether stepping down the SMB version ladder
// could plausibly fix this failure. Bad credentials and missing shares are
// not dialect problems, and retrying them three times just makes the common
// typo case three times slower (SPEC.md §5).
func (e *MountError) DialectRetryable() bool {
	return e.Kind == KindDialect || e.Kind == KindUnknown
}

// mountErrno extracts N from mount.cifs's "mount error(N): ..." output.
var mountErrnoRE = regexp.MustCompile(`mount error\s*\(?(\d+)\)?`)

// classifyMountFailure turns an exit code plus mount.cifs output into a
// MountError. ctxErr, when set, means our own deadline fired first and takes
// precedence over whatever the process managed to print.
func classifyMountFailure(source string, exitCode int, detail string, ctxErr error) error {
	e := &MountError{Source: source, ExitCode: exitCode, Detail: detail, Kind: KindUnknown}

	if ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			e.Kind = KindTimeout
			return e
		}
		// Cancellation is the caller's doing, not a mount problem; surface it
		// unwrapped so callers can test for it.
		return fmt.Errorf("mounting %s was cancelled: %w", source, ctxErr)
	}

	if m := mountErrnoRE.FindStringSubmatch(detail); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			e.Errno = n
			e.Kind = kindForErrno(n)
		}
	}

	if e.Kind == KindUnknown {
		e.Kind = kindForText(detail)
	}
	return e
}

// kindForErrno maps the errno mount.cifs reports onto a failure kind.
func kindForErrno(n int) FailureKind {
	switch n {
	case 13: // EACCES
		return KindAuth
	case 1: // EPERM — mount not permitted, or the server refused the session
		return KindPermission
	case 2, 6: // ENOENT, ENXIO — no such share
		return KindShareMissing
	case 110, 111, 112, 113: // ETIMEDOUT, ECONNREFUSED, EHOSTDOWN, EHOSTUNREACH
		return KindUnreachable
	case 22, 95, 524: // EINVAL, EOPNOTSUPP, ENOTSUPP — dialect/option rejected
		return KindDialect
	default:
		return KindUnknown
	}
}

// kindForText is the fallback for messages that carry no errno, notably the
// address-resolution failures mount.cifs reports before it ever connects.
func kindForText(detail string) FailureKind {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "unable to find suitable address"),
		strings.Contains(d, "could not resolve address"),
		strings.Contains(d, "no address associated"),
		strings.Contains(d, "connection timed out"),
		strings.Contains(d, "host is down"),
		strings.Contains(d, "network is unreachable"),
		strings.Contains(d, "no route to host"):
		return KindUnreachable
	case strings.Contains(d, "permission denied"),
		strings.Contains(d, "password"):
		return KindAuth
	case strings.Contains(d, "operation not permitted"),
		strings.Contains(d, "must be superuser"),
		strings.Contains(d, "only root can"):
		return KindPermission
	case strings.Contains(d, "bad unc"), strings.Contains(d, "invalid argument"):
		return KindDialect
	default:
		return KindUnknown
	}
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return strings.TrimSpace(s)
}
