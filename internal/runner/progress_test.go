package runner

import (
	"fmt"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/engine"
)

// actionsOf is what a confirm screen renders, so its failure mode is a user
// approving destructive work they were not shown. The two cases that matter
// are the ones the projection is easy to get wrong: Unblock removals sharing
// an ActionKind with ordinary deletes, and truncation.
func TestActionsOfSeparatesUnblocksFromDeletes(t *testing.T) {
	plan := &engine.Plan{Actions: []engine.Action{
		// An unblock removal — same Kind as a real delete, different meaning.
		{Kind: engine.ActionDelete, RelPath: "in-the-way", Unblock: true,
			Reason: "a symlink is where the source has a real file"},
		{Kind: engine.ActionRmDir, RelPath: "dir-in-the-way", Unblock: true},
		{Kind: engine.ActionMkDir, RelPath: "new/dir"},
		{Kind: engine.ActionCopy, RelPath: "new/file.txt", Size: 10},
		{Kind: engine.ActionCopy, RelPath: "changed.txt", Size: 20, Overwrite: true},
		{Kind: engine.ActionDelete, RelPath: "gone.txt", Size: 30},
		{Kind: engine.ActionRmDir, RelPath: "empty"},
	}}

	got := actionsOf("dest-1", plan)

	if len(got.Deletes) != 1 || got.Deletes[0].RelPath != "gone.txt" {
		t.Fatalf("deletes = %+v, want only gone.txt — an unblock removal must not be folded in", got.Deletes)
	}
	if len(got.RmDirs) != 1 || got.RmDirs[0] != "empty" {
		t.Fatalf("rmdirs = %v, want only empty", got.RmDirs)
	}
	if len(got.Replaces) != 2 {
		t.Fatalf("replaces = %+v, want both unblock removals", got.Replaces)
	}
	if len(got.MkDirs) != 1 || len(got.Copies) != 2 {
		t.Fatalf("mkdirs = %v, copies = %+v", got.MkDirs, got.Copies)
	}
	if !got.Copies[1].Overwrite {
		t.Error("an overwriting copy lost its overwrite flag; the screen would call it a new file")
	}
	if got.Truncated {
		t.Error("a seven-action plan reported truncation")
	}
}

// An unblock on a kind other than delete/rmdir must still be reported as a
// removal. The differ does not produce one today, but the projection is what
// stands between that changing and a removal being displayed as "will be
// created" — so it is checked here rather than assumed.
func TestActionsOfTreatsAnyUnblockAsARemoval(t *testing.T) {
	plan := &engine.Plan{Actions: []engine.Action{
		{Kind: engine.ActionMkDir, RelPath: "surprise", Unblock: true},
	}}

	got := actionsOf("dest-1", plan)

	if len(got.Replaces) != 1 {
		t.Fatalf("replaces = %+v, want the unblocked action", got.Replaces)
	}
	if len(got.MkDirs) != 0 {
		t.Fatalf("an unblock was filed under mkdirs (%v), hiding a removal", got.MkDirs)
	}
}

func TestActionsOfTruncatesEachList(t *testing.T) {
	var actions []engine.Action
	for i := range planPathLimit + 5 {
		actions = append(actions, engine.Action{
			Kind: engine.ActionDelete, RelPath: fmt.Sprintf("d%05d", i), Size: 1,
		})
	}
	plan := &engine.Plan{Actions: actions}

	got := actionsOf("dest-1", plan)

	if len(got.Deletes) != planPathLimit {
		t.Fatalf("deletes = %d, want capped at %d", len(got.Deletes), planPathLimit)
	}
	if !got.Truncated {
		t.Fatal("a capped list did not report truncation; the UI would show it as the whole list")
	}
	// The cap keeps the first entries in the differ's order, which is
	// deepest-first — not an arbitrary subset.
	if got.Deletes[0].RelPath != "d00000" {
		t.Fatalf("first retained delete = %q, want the first in plan order", got.Deletes[0].RelPath)
	}
}

// A nil plan is what a destination that failed before diffing has. It must
// project to empty lists rather than panicking, and must not look truncated.
func TestActionsOfHandlesANilPlan(t *testing.T) {
	got := actionsOf("dest-1", nil)

	if got.DestTargetID != "dest-1" {
		t.Fatalf("dest_target_id = %q", got.DestTargetID)
	}
	if got.Deletes == nil || got.RmDirs == nil || got.Replaces == nil ||
		got.Copies == nil || got.MkDirs == nil {
		t.Fatal("nil slices would serialise as JSON null; the UI expects arrays")
	}
	if len(got.Deletes) != 0 || got.Truncated {
		t.Fatal("a nil plan produced actions")
	}
}

// The count the screen prints beside a list comes from DestPlan; the list
// comes from DestActions. If they disagree the screen lies, so the projection
// of both from one engine.Plan is checked together.
func TestDestPlanCountsMatchTheActionLists(t *testing.T) {
	plan := &engine.Plan{
		Actions: []engine.Action{
			{Kind: engine.ActionDelete, RelPath: "a"},
			{Kind: engine.ActionDelete, RelPath: "b"},
			{Kind: engine.ActionDelete, RelPath: "unblock", Unblock: true},
			{Kind: engine.ActionRmDir, RelPath: "d"},
		},
		Deletes:  2,
		RmDirs:   1,
		Unblocks: 1,
	}

	dp := destPlanOf("dest-1", plan)
	da := actionsOf("dest-1", plan)

	if dp.Deletes != len(da.Deletes) {
		t.Fatalf("DestPlan.Deletes = %d but the list holds %d", dp.Deletes, len(da.Deletes))
	}
	if dp.RmDirs != len(da.RmDirs) {
		t.Fatalf("DestPlan.RmDirs = %d but the list holds %d", dp.RmDirs, len(da.RmDirs))
	}
	if dp.Replaces != len(da.Replaces) {
		t.Fatalf("DestPlan.Replaces = %d but the list holds %d", dp.Replaces, len(da.Replaces))
	}
}
