package mountmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClassifyMountFailure(t *testing.T) {
	const source = "//192.168.1.50/media"

	tests := []struct {
		name          string
		detail        string
		ctxErr        error
		wantKind      FailureKind
		wantRetryable bool
		wantInMessage string
	}{
		{
			name:          "bad credentials",
			detail:        mountErrnoOutput(13, "Permission denied"),
			wantKind:      KindAuth,
			wantRetryable: false,
			wantInMessage: "username, password and domain",
		},
		{
			name:          "share does not exist",
			detail:        mountErrnoOutput(2, "No such file or directory"),
			wantKind:      KindShareMissing,
			wantRetryable: false,
			wantInMessage: "does not exist",
		},
		{
			name:          "host is down",
			detail:        mountErrnoOutput(112, "Host is down"),
			wantKind:      KindUnreachable,
			wantRetryable: false,
			wantInMessage: "cannot reach",
		},
		{
			name:          "connection timed out",
			detail:        mountErrnoOutput(110, "Connection timed out"),
			wantKind:      KindUnreachable,
			wantInMessage: "cannot reach",
		},
		{
			name:          "no route to host",
			detail:        mountErrnoOutput(113, "No route to host"),
			wantKind:      KindUnreachable,
			wantInMessage: "cannot reach",
		},
		{
			name:          "dialect rejected",
			detail:        mountErrnoOutput(95, "Operation not supported"),
			wantKind:      KindDialect,
			wantRetryable: true,
			wantInMessage: "SMB version",
		},
		{
			name:          "invalid argument is treated as a dialect problem",
			detail:        mountErrnoOutput(22, "Invalid argument"),
			wantKind:      KindDialect,
			wantRetryable: true,
		},
		{
			name:          "address cannot be resolved",
			detail:        "mount error: could not resolve address for nosuchhost",
			wantKind:      KindUnreachable,
			wantInMessage: "cannot reach",
		},
		{
			name:          "unable to find suitable address",
			detail:        "Unable to find suitable address.",
			wantKind:      KindUnreachable,
			wantInMessage: "cannot reach",
		},
		{
			name:          "not permitted to mount at all",
			detail:        "mount: only root can use \"--options\" option",
			wantKind:      KindPermission,
			wantInMessage: "SYS_ADMIN",
		},
		{
			name:          "our own deadline wins over the process output",
			detail:        mountErrnoOutput(13, "Permission denied"),
			ctxErr:        context.DeadlineExceeded,
			wantKind:      KindTimeout,
			wantInMessage: "timed out",
		},
		{
			name:          "unrecognised output keeps the raw first line",
			detail:        "something entirely unexpected happened\nsecond line",
			wantKind:      KindUnknown,
			wantRetryable: true,
			wantInMessage: "something entirely unexpected happened",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyMountFailure(source, 32, tt.detail, tt.ctxErr)

			var me *MountError
			if !errors.As(err, &me) {
				t.Fatalf("classify returned %T, want *MountError", err)
			}
			if me.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", me.Kind, tt.wantKind)
			}
			if me.DialectRetryable() != tt.wantRetryable {
				t.Errorf("DialectRetryable() = %v, want %v", me.DialectRetryable(), tt.wantRetryable)
			}
			if tt.wantInMessage != "" && !strings.Contains(me.Error(), tt.wantInMessage) {
				t.Errorf("message %q does not contain %q", me.Error(), tt.wantInMessage)
			}
			if !strings.Contains(me.Error(), source) {
				t.Errorf("message %q does not name the share", me.Error())
			}
		})
	}
}

// Error strings reach the UI, so they must not read like Go internals
// (CLAUDE.md, Code conventions).
func TestMountErrorMessagesAreLegible(t *testing.T) {
	forbidden := []string{"exec.ExitError", "*errors.errorString", "0x", "goroutine"}

	for _, kind := range []FailureKind{KindAuth, KindShareMissing, KindUnreachable, KindTimeout, KindDialect, KindPermission} {
		e := &MountError{Source: "//192.168.1.50/media", Kind: kind}
		msg := e.Error()

		if msg == "" {
			t.Fatalf("kind %q produced an empty message", kind)
		}
		if strings.ToUpper(msg[:1]) == msg[:1] && !strings.HasPrefix(msg, "//") {
			t.Errorf("message %q should start lowercase so it reads correctly when wrapped", msg)
		}
		for _, f := range forbidden {
			if strings.Contains(msg, f) {
				t.Errorf("message %q leaks Go internals (%q)", msg, f)
			}
		}
	}
}

func TestClassifyCancellationIsNotAMountError(t *testing.T) {
	err := classifyMountFailure("//h/s", 32, "", context.Canceled)

	var me *MountError
	if errors.As(err, &me) {
		t.Fatalf("cancellation was classified as a mount failure: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", err)
	}
}
