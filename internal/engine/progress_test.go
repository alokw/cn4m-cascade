package engine

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestETAIsUnknownBeforeThereIsAnythingToGoOn(t *testing.T) {
	start := time.Now()
	tr := NewTracker(start)

	if got := tr.Snapshot(start).ETASeconds; got != ETAUnknown {
		t.Errorf("ETA during scanning = %v, want unknown", got)
	}

	tr.SetPhase(PhaseCopying)
	tr.SetTotals(10, 1000)
	if got := tr.Snapshot(start).ETASeconds; got != ETAUnknown {
		t.Errorf("ETA before any throughput = %v, want unknown", got)
	}
}

func TestScanProgressIsReportedBeforeTotalsExist(t *testing.T) {
	start := time.Now()
	tr := NewTracker(start)
	tr.SetScanProgress(1234, 56)

	snap := tr.Snapshot(start)
	if snap.FilesFound != 1234 || snap.DirsFound != 56 {
		t.Errorf("scan progress = %d files / %d dirs, want 1234 / 56", snap.FilesFound, snap.DirsFound)
	}
	if snap.Phase != PhaseScanning {
		t.Errorf("phase = %q, want %q", snap.Phase, PhaseScanning)
	}
}

// SPEC.md §12: total ETA error under ~20% once 25% of the bytes are done.
func TestETAConverges(t *testing.T) {
	const (
		totalBytes = 1 << 30 // 1 GiB
		rate       = 50 << 20
		tick       = 500 * time.Millisecond
	)
	trueTotalSec := float64(totalBytes) / rate

	tests := []struct {
		name string
		// chunk returns the bytes transferred in one tick.
		chunk func(rng *rand.Rand) int64
	}{
		{
			name:  "steady throughput",
			chunk: func(*rand.Rand) int64 { return int64(rate * tick.Seconds()) },
		},
		{
			// A tree of mixed file sizes makes throughput lumpy; the point
			// of the EWMA is that the ETA does not whiplash.
			name: "lumpy throughput averaging the same rate",
			chunk: func(rng *rand.Rand) int64 {
				factor := 0.5 + rng.Float64() // 0.5x to 1.5x, mean 1.0
				return int64(rate * tick.Seconds() * factor)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(1))
			start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

			tr := NewTracker(start)
			tr.SetPhase(PhaseCopying)
			tr.SetTotals(1000, totalBytes)

			var (
				done    int64
				now     = start
				checked bool
			)
			for done < totalBytes {
				n := tt.chunk(rng)
				if done+n > totalBytes {
					n = totalBytes - done
				}
				done += n
				now = now.Add(tick)

				tr.AddFileBytes("f", n)
				tr.Sample(now)

				// Check once, the first time we pass the 25% mark.
				if !checked && done >= totalBytes/4 {
					checked = true

					snap := tr.Snapshot(now)
					if snap.ETASeconds == ETAUnknown {
						t.Fatal("ETA is still unknown at 25% of bytes")
					}

					elapsed := now.Sub(start).Seconds()
					trueRemaining := trueTotalSec - elapsed
					relErr := math.Abs(snap.ETASeconds-trueRemaining) / trueRemaining

					t.Logf("at %.0f%%: ETA %.1fs, true %.1fs, error %.1f%%, throughput %.1f MiB/s",
						float64(done)/totalBytes*100, snap.ETASeconds, trueRemaining,
						relErr*100, snap.ThroughputBPS/(1<<20))

					if relErr > 0.20 {
						t.Errorf("ETA error %.1f%% exceeds the 20%% budget", relErr*100)
					}
				}
			}
		})
	}
}

// ETA must come from bytes, not file counts: a tree of one huge file and many
// tiny ones would mislead badly the other way (SPEC.md §6.1.1).
func TestETAUsesBytesNotFileCounts(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tr := NewTracker(start)
	tr.SetPhase(PhaseCopying)

	// 100 files, but almost all the bytes are in the last one.
	tr.SetTotals(100, 1_000_000_000)

	now := start
	for range 99 {
		tr.FinishFile("tiny", true)
	}
	// 99 of 100 files done, but only 1% of the bytes.
	tr.AddFileBytes("tiny", 10_000_000)
	now = now.Add(time.Second)
	tr.Sample(now)

	snap := tr.Snapshot(now)
	if snap.ETASeconds == ETAUnknown {
		t.Fatal("ETA unknown despite observed throughput")
	}
	// At 10 MB/s with 990 MB left, the honest answer is ~99s — not the
	// "almost done" a file count would suggest.
	if snap.ETASeconds < 50 {
		t.Errorf("ETA = %.1fs; a byte-based estimate should be far larger with 99%% of bytes left", snap.ETASeconds)
	}
}

func TestTrackerInFlightFiles(t *testing.T) {
	start := time.Now()
	tr := NewTracker(start)
	tr.SetPhase(PhaseCopying)
	tr.SetTotals(2, 3000)

	tr.StartFile("big.bin", 2000, start)
	tr.StartFile("small.bin", 1000, start)
	tr.AddFileBytes("big.bin", 500)

	snap := tr.Snapshot(start)
	if len(snap.InFlight) != 2 {
		t.Fatalf("in flight = %d, want 2", len(snap.InFlight))
	}
	if snap.InFlight[0].RelPath != "big.bin" {
		t.Errorf("in-flight files are not sorted: %+v", snap.InFlight)
	}
	if got := snap.InFlight[0].Percent; math.Abs(got-25) > 0.01 {
		t.Errorf("big.bin percent = %.2f, want 25", got)
	}

	tr.FinishFile("big.bin", true)
	if snap := tr.Snapshot(start); len(snap.InFlight) != 1 {
		t.Errorf("finished file still in flight: %+v", snap.InFlight)
	}
}

// A failed attempt's bytes are rewound, and must not push totals negative.
func TestTrackerRewindsBytes(t *testing.T) {
	start := time.Now()
	tr := NewTracker(start)
	tr.StartFile("f", 1000, start)

	tr.AddFileBytes("f", 400)
	tr.AddFileBytes("f", -400)
	if got := tr.Snapshot(start).BytesDone; got != 0 {
		t.Errorf("bytes done = %d, want 0", got)
	}

	tr.AddFileBytes("f", -999)
	if got := tr.Snapshot(start).BytesDone; got != 0 {
		t.Errorf("bytes done = %d, want 0; progress must not go negative", got)
	}
}

func TestTrackerCountsOutcomes(t *testing.T) {
	start := time.Now()
	tr := NewTracker(start)

	tr.FinishFile("a", true)
	tr.FinishFile("b", true)
	tr.FinishFile("c", false)
	tr.AddDeleted(5)

	snap := tr.Snapshot(start)
	if snap.FilesDone != 2 {
		t.Errorf("files done = %d, want 2", snap.FilesDone)
	}
	if snap.FilesFailed != 1 {
		t.Errorf("files failed = %d, want 1", snap.FilesFailed)
	}
	if snap.FilesDeleted != 5 {
		t.Errorf("files deleted = %d, want 5", snap.FilesDeleted)
	}
}
