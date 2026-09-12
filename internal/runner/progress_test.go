package runner

import (
	"fmt"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
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

// A fan-out run must not count upwards as each destination is planned.
//
// Reported 2026-09-10: copying one file to seven destinations counted "3/5",
// then "6/7", then "7/7". The total summed only *planned* destinations, and with
// parallel_destinations off they are planned one at a time, so the denominator
// climbed as the run went — reading as the job growing while it worked.
// SPEC.md §6.1.1 asks the run total to include destinations that have not
// started, estimated from the ones that have.
func TestFanOutTotalCoversDestinationsNotYetPlanned(t *testing.T) {
	now := time.Now()
	dests := []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7"}
	p := newProgress(now, dests)

	// One destination planned: 1 file, 1000 bytes of real work.
	tr := p.tracker("d1")
	tr.SetTotals(1, 1000)
	p.markPlanned("d1")
	p.setStatus("d1", store.DestRunning)
	for _, id := range dests[1:] {
		p.setStatus(id, store.DestPending)
	}

	snap := p.snapshot(now)
	if snap.FilesTotal != 7 {
		t.Fatalf("files_total = %d, want 7: one planned destination of 1 file plus six pending like it",
			snap.FilesTotal)
	}
	if snap.BytesTotal != 7000 {
		t.Fatalf("bytes_total = %d, want 7000", snap.BytesTotal)
	}

	// Planning the rest must not move the total, only fill in what was estimated.
	for _, id := range dests[1:] {
		tr := p.tracker(id)
		tr.SetTotals(1, 1000)
		p.markPlanned(id)
		p.setStatus(id, store.DestRunning)
	}
	if got := p.snapshot(now).FilesTotal; got != 7 {
		t.Fatalf("files_total = %d after planning them all, want a steady 7", got)
	}
}

// The regression that made this file worth rewriting.
//
// An earlier fix set the total from the *source scan* — files × destinations,
// fixed when the scan ended. It was stable and wildly wrong: a tree already in
// sync reported its entire size as the transfer. Reported 2026-09-11 as
// "multiple terabytes for a few 100mb files".
//
// The denominator must be the **work**, never the scope.
func TestTotalsReportWorkNotScope(t *testing.T) {
	now := time.Now()
	dests := []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7"}
	p := newProgress(now, dests)

	const mib = 1 << 20
	// Every destination is planned, and each has one 100 MiB file to copy —
	// out of a source tree that is hundreds of gigabytes and otherwise in sync.
	for _, id := range dests {
		tr := p.tracker(id)
		tr.SetTotals(1, 100*mib)
		p.markPlanned(id)
		p.setStatus(id, store.DestRunning)
	}

	snap := p.snapshot(now)
	wantBytes := int64(7 * 100 * mib)
	if snap.BytesTotal != wantBytes {
		t.Fatalf("bytes_total = %d (%.1f GB), want %d (%.0f MB) — the total must be what is copied, not what was scanned",
			snap.BytesTotal, float64(snap.BytesTotal)/(1<<30), wantBytes, float64(wantBytes)/mib)
	}
	if snap.FilesTotal != 7 {
		t.Fatalf("files_total = %d, want 7", snap.FilesTotal)
	}
}

// A destination that never ran leaves nothing behind in the estimate: it was
// never planned, so it contributes no work and no pending share once it has
// reached a terminal state.
func TestSkippedDestinationDoesNotInflateTheTotal(t *testing.T) {
	now := time.Now()
	p := newProgress(now, []string{"d1", "d2"})

	tr := p.tracker("d1")
	tr.SetTotals(2, 200)
	p.markPlanned("d1")
	p.setStatus("d1", store.DestRunning)
	// d2 is unreachable and the job skips it: not pending any more.
	p.setStatus("d2", store.DestSkippedUnavailable)

	snap := p.snapshot(now)
	if snap.FilesTotal != 2 {
		t.Fatalf("files_total = %d, want 2 — a skipped destination adds no work", snap.FilesTotal)
	}
	if snap.BytesTotal != 200 {
		t.Fatalf("bytes_total = %d, want 200", snap.BytesTotal)
	}
}

// The ETA must not count pending destinations twice: the estimate is already
// folded into BytesTotal.
func TestETADoesNotDoubleCountPendingDestinations(t *testing.T) {
	now := time.Now()
	p := newProgress(now, []string{"d1", "d2"})

	tr := p.tracker("d1")
	tr.SetTotals(1, 1000)
	p.markPlanned("d1")
	p.setStatus("d1", store.DestRunning)
	p.setStatus("d2", store.DestPending)

	snap := p.snapshot(now)
	if snap.BytesTotal != 2000 {
		t.Fatalf("bytes_total = %d, want 2000 (one planned + one estimated)", snap.BytesTotal)
	}
	if snap.EstimatedPendingBytes != 1000 {
		t.Fatalf("estimated_pending_bytes = %d, want 1000", snap.EstimatedPendingBytes)
	}
	// BytesTotal already includes the estimate, so remaining is 2000, not 3000.
	if snap.ThroughputBPS > 0 {
		t.Fatalf("unexpected throughput in a synthetic snapshot: %v", snap.ThroughputBPS)
	}
}

// The scan size is reported for context, and must never become the progress
// denominator. Keeping both in one test makes the distinction hard to erode:
// D-130 was exactly the mistake of letting the second become the first.
func TestScanSizeIsReportedButIsNotTheDenominator(t *testing.T) {
	now := time.Now()
	p := newProgress(now, []string{"d1", "d2"})

	const mib = 1 << 20
	// A large tree that is almost entirely in sync: 500 files, 5 GB scanned,
	// but only one 10 MiB file actually needs copying to each destination.
	p.setSourceScan(500, 5000*mib)
	for _, id := range []string{"d1", "d2"} {
		tr := p.tracker(id)
		tr.SetTotals(1, 10*mib)
		p.markPlanned(id)
		p.setStatus(id, store.DestRunning)
	}

	snap := p.snapshot(now)

	if snap.ScannedBytes != 5000*mib || snap.ScannedFiles != 500 {
		t.Fatalf("scanned = %d files / %d bytes, want 500 / %d", snap.ScannedFiles, snap.ScannedBytes, 5000*mib)
	}
	// The transfer is 2 x 10 MiB. If the scan ever leaks into this, it reads
	// as 10 GB and the progress bar becomes meaningless.
	if snap.BytesTotal != 2*10*mib {
		t.Fatalf("bytes_total = %d (%.1f GB), want %d (20 MiB) — the scan size must not be the denominator",
			snap.BytesTotal, float64(snap.BytesTotal)/(1<<30), 2*10*mib)
	}
	if snap.FilesTotal != 2 {
		t.Fatalf("files_total = %d, want 2", snap.FilesTotal)
	}
}
