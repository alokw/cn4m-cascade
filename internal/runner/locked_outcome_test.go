package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

func lockedFailure(relPath string) engine.Failure {
	return engine.Failure{
		RelPath: relPath,
		Kind:    engine.ActionCopy,
		Err:     fmt.Errorf("%s: %w", relPath, engine.ErrDestinationLocked),
	}
}

// Reported 2026-09-11: a fan-out to seven media servers reported `failed` on
// every run, because the same clip was open on all seven — media servers hold
// their project files open for as long as the project is loaded.
//
// A destination that copied everything except files somebody else had open is
// **degraded, not failed**. Not a clean success either: it is genuinely not in
// sync. The distinction matters because `failed` on a condition nobody can
// clear teaches people to ignore the status, and then a real failure is missed
// too.
func TestLockedOnlyDestinationIsDegradedNotFailed(t *testing.T) {
	tests := []struct {
		name         string
		failures     []engine.Failure
		wantStatus   store.DestStatus
		wantDegraded bool
	}{
		{
			name:         "only locked files",
			failures:     []engine.Failure{lockedFailure("1600/clip.mov")},
			wantStatus:   store.DestSuccess,
			wantDegraded: true,
		},
		{
			name: "several locked files, still degraded",
			failures: []engine.Failure{
				lockedFailure("a.mov"), lockedFailure("b.mov"), lockedFailure("c.mov"), lockedFailure("d.mov"),
			},
			wantStatus:   store.DestSuccess,
			wantDegraded: true,
		},
		{
			// One real error among them means the destination really failed.
			name: "a genuine failure alongside a lock is still a failure",
			failures: []engine.Failure{
				lockedFailure("a.mov"),
				{RelPath: "b.mov", Kind: engine.ActionCopy, Err: errors.New("disk full")},
			},
			wantStatus:   store.DestFailed,
			wantDegraded: false,
		},
		{
			name:         "a genuine failure alone",
			failures:     []engine.Failure{{RelPath: "b.mov", Kind: engine.ActionCopy, Err: errors.New("disk full")}},
			wantStatus:   store.DestFailed,
			wantDegraded: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := &engine.ExecResult{Failures: tc.failures}
			status, summary, degraded := classifyDestination(result, &engine.Plan{})

			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (summary %q)", status, tc.wantStatus, summary)
			}
			if degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", degraded, tc.wantDegraded)
			}
			if summary == "" {
				t.Error("empty summary; the operator has to be told what happened")
			}
		})
	}
}

// The guard that keeps this safe: locked files remain failures as far as the
// deletion guard is concerned, because a destination missing a file it was
// meant to receive must not have deletions run against it.
func TestLockedFilesStillBlockDeletions(t *testing.T) {
	result := &engine.ExecResult{Failures: []engine.Failure{lockedFailure("1600/clip.mov")}}

	if !result.Failed() {
		t.Fatal("a locked file no longer counts as a copy failure; mirror deletions would run against a destination that is not in sync")
	}
}

// The message has to say what happens next, because "locked" on its own does
// not tell an operator whether to do something.
func TestLockedSummaryNamesFilesAndSaysWhatHappensNext(t *testing.T) {
	summary := summariseLocked([]engine.Failure{
		lockedFailure("one.mov"), lockedFailure("two.mov"), lockedFailure("three.mov"),
		lockedFailure("four.mov"), lockedFailure("five.mov"),
	})

	for _, want := range []string{"one.mov", "two.mov", "three.mov", "and 2 more", "next run"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}
	if strings.Contains(summary, "four.mov") {
		t.Errorf("summary lists more than a handful of names: %q", summary)
	}
}
