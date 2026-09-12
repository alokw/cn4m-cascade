package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// A locked destination must never be retried. Retrying spends the run's budget
// waiting on a file whose owner decides when it is free (SPEC.md §6.2).
func TestDestinationLockedIsNotRetryable(t *testing.T) {
	err := fmt.Errorf("%s: %w", "dst/file.mov", ErrDestinationLocked)

	if retryable(err) {
		t.Fatal("a locked destination is retryable; it must not be")
	}
	if !errors.Is(err, ErrDestinationLocked) {
		t.Fatal("the wrapped error no longer matches ErrDestinationLocked")
	}
}

// The individual log lines are not enough on their own: a run of thousands of
// files buries three locked ones, and the operator has to know at a glance that
// the destination is not fully in sync.
func TestLockedFilesAreSummarisedAtCompletion(t *testing.T) {
	tests := []struct {
		name        string
		locked      []string
		otherFails  int
		wantSummary bool
		wantNamed   []string
		wantPhrase  string
	}{
		{
			name:        "no locked files, no summary",
			otherFails:  2,
			wantSummary: false,
		},
		{
			name:        "one locked file is named",
			locked:      []string{"1200/a.mov"},
			wantSummary: true,
			wantNamed:   []string{"1200/a.mov"},
			wantPhrase:  "1 file(s) were not copied",
		},
		{
			name:        "a handful are all named",
			locked:      []string{"a.mov", "b.mov", "c.mov"},
			wantSummary: true,
			wantNamed:   []string{"a.mov", "b.mov", "c.mov"},
			wantPhrase:  "3 file(s) were not copied",
		},
		{
			// Past a handful the list stops being readable, and the individual
			// entries are the record.
			name:        "more than five are capped with a count",
			locked:      []string{"a", "b", "c", "d", "e", "f", "g"},
			wantSummary: true,
			wantNamed:   []string{"a", "b", "c", "d", "e"},
			wantPhrase:  "and 2 more",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []Event
			e := &Executor{OnEvent: func(ev Event) { events = append(events, ev) }}

			result := &ExecResult{}
			for _, p := range tc.locked {
				result.Failures = append(result.Failures, Failure{
					RelPath: p, Kind: ActionCopy,
					Err: fmt.Errorf("%s: %w", p, ErrDestinationLocked),
				})
			}
			for i := 0; i < tc.otherFails; i++ {
				result.Failures = append(result.Failures, Failure{
					RelPath: fmt.Sprintf("other-%d", i), Kind: ActionCopy,
					Err: errors.New("something else went wrong"),
				})
			}

			e.summariseLockedFiles(result)

			if !tc.wantSummary {
				if len(events) != 0 {
					t.Fatalf("expected no summary, got %+v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("expected exactly one summary event, got %d: %+v", len(events), events)
			}
			ev := events[0]
			if ev.Level != store.LevelWarn {
				t.Errorf("level = %v, want warn: a partially synced destination is not routine", ev.Level)
			}
			if !strings.Contains(ev.Message, tc.wantPhrase) {
				t.Errorf("message %q does not contain %q", ev.Message, tc.wantPhrase)
			}
			for _, name := range tc.wantNamed {
				if !strings.Contains(ev.Message, name) {
					t.Errorf("message %q does not name %q", ev.Message, name)
				}
			}
			// The operator needs to know it will not be retried, or they will
			// wait for a retry that never comes.
			if !strings.Contains(ev.Message, "not retried") {
				t.Errorf("message %q does not say the files are not retried", ev.Message)
			}
		})
	}
}

// Off Windows there is no such condition: a rename over an open file succeeds.
// Asserting it keeps a future change from quietly making Linux behave like
// Windows, which would mean skipping files that copy perfectly well today.
func TestLockedByAnotherProcessIsPlatformSpecific(t *testing.T) {
	for _, err := range []error{
		errors.New("permission denied"),
		fmt.Errorf("wrapped: %w", errors.New("sharing violation")),
	} {
		if got := lockedByAnotherProcess(err); got != wantLockedForPlatform {
			t.Errorf("lockedByAnotherProcess(%v) = %v, want %v", err, got, wantLockedForPlatform)
		}
	}
}
