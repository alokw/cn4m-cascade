package engine

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Phase is what a run is currently doing.
type Phase string

const (
	PhaseScanning Phase = "scanning"
	PhasePlanning Phase = "planning"
	PhaseCopying  Phase = "copying"
	PhaseDeleting Phase = "deleting"
	PhaseDone     Phase = "done"
)

// ewmaTau is the smoothing constant for throughput. SPEC.md §6.1.1 asks for a
// rolling average over roughly 15 seconds so the ETA does not whiplash
// between a run of large files and a run of small ones.
const ewmaTau = 15 * time.Second

// ETAUnknown is reported while there is not yet enough information — during
// the scan, or before any throughput has been observed.
const ETAUnknown = -1

// FileInFlight is one file currently being copied.
type FileInFlight struct {
	RelPath    string  `json:"relpath"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	Percent    float64 `json:"percent"`
	// ThroughputBPS is this file's own rate, not the run's: a small file
	// queued behind a large one would otherwise report a nonsense ETA.
	ThroughputBPS float64 `json:"throughput_bps"`
	ETASeconds    float64 `json:"eta_sec"`
}

// Snapshot is a consistent view of a run's progress.
type Snapshot struct {
	Phase Phase `json:"phase"`

	FilesTotal   int64 `json:"files_total"`
	FilesDone    int64 `json:"files_done"`
	FilesDeleted int64 `json:"files_deleted"`
	FilesFailed  int64 `json:"files_failed"`
	BytesTotal   int64 `json:"bytes_total"`
	BytesDone    int64 `json:"bytes_done"`

	// DirsFound and FilesFound are what the scan has discovered so far,
	// reported before totals are known.
	FilesFound int64 `json:"files_found"`
	DirsFound  int64 `json:"dirs_found"`

	ThroughputBPS float64 `json:"throughput_bps"`
	ETASeconds    float64 `json:"eta_sec"`
	ElapsedSec    float64 `json:"elapsed_sec"`

	InFlight []FileInFlight `json:"in_flight"`
}

// Tracker accumulates progress for one destination and derives throughput and
// ETA from it (SPEC.md §6.1.1).
//
// Time is passed in rather than read from the clock so that the ETA logic is
// testable without sleeping.
type Tracker struct {
	mu sync.Mutex

	started time.Time
	phase   Phase

	filesTotal   int64
	filesDone    int64
	filesDeleted int64
	filesFailed  int64
	bytesTotal   int64
	bytesDone    int64

	filesFound int64
	dirsFound  int64

	// EWMA state.
	throughput float64
	lastSample time.Time
	lastBytes  int64

	inFlight map[string]*fileState
}

type fileState struct {
	total   int64
	done    int64
	started time.Time
}

// nowFunc is the clock the engine reads. Tests replace it.
var nowFunc = time.Now

// NewTracker starts a tracker at the given moment.
func NewTracker(now time.Time) *Tracker {
	return &Tracker{
		started:    now,
		phase:      PhaseScanning,
		lastSample: now,
		inFlight:   map[string]*fileState{},
	}
}

// SetPhase records what the run is doing now.
func (t *Tracker) SetPhase(p Phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase = p
}

// SetScanProgress records what the scan has discovered so far.
func (t *Tracker) SetScanProgress(files, dirs int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.filesFound, t.dirsFound = files, dirs
}

// SetTotals fixes the denominators once the plan is known.
func (t *Tracker) SetTotals(files, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.filesTotal, t.bytesTotal = files, bytes
}

// StartFile marks a file as in flight.
func (t *Tracker) StartFile(relPath string, size int64, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inFlight[relPath] = &fileState{total: size, started: now}
}

// AddFileBytes records progress within one file and towards the run total.
// A negative count rewinds bytes that a failed attempt discarded.
func (t *Tracker) AddFileBytes(relPath string, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.bytesDone += n
	if t.bytesDone < 0 {
		t.bytesDone = 0
	}
	if f, ok := t.inFlight[relPath]; ok {
		f.done += n
		if f.done < 0 {
			f.done = 0
		}
	}
}

// FinishFile removes a file from the in-flight set and counts it done.
func (t *Tracker) FinishFile(relPath string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.inFlight, relPath)
	if ok {
		t.filesDone++
	} else {
		t.filesFailed++
	}
}

// AddDeleted counts removals, which have no bytes.
func (t *Tracker) AddDeleted(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.filesDeleted += n
}

// Sample folds the bytes seen since the last call into the rolling
// throughput. Call it at a steady cadence — the run loop does so at ~2 Hz.
func (t *Tracker) Sample(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	elapsed := now.Sub(t.lastSample)
	if elapsed <= 0 {
		return
	}

	delta := t.bytesDone - t.lastBytes
	if delta < 0 {
		delta = 0
	}
	instant := float64(delta) / elapsed.Seconds()

	// Time-based smoothing: the weight depends on how long this sample
	// covered, so an irregular cadence does not distort the average.
	alpha := 1 - math.Exp(-elapsed.Seconds()/ewmaTau.Seconds())
	if t.throughput == 0 && t.lastBytes == 0 && delta > 0 {
		// Seed with the first real observation rather than easing up from
		// zero, which would make the first ETA wildly pessimistic.
		t.throughput = instant
	} else {
		t.throughput += alpha * (instant - t.throughput)
	}

	t.lastSample, t.lastBytes = now, t.bytesDone
}

// Snapshot returns a consistent view for the API and the logs.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := Snapshot{
		Phase:         t.phase,
		FilesTotal:    t.filesTotal,
		FilesDone:     t.filesDone,
		FilesDeleted:  t.filesDeleted,
		FilesFailed:   t.filesFailed,
		BytesTotal:    t.bytesTotal,
		BytesDone:     t.bytesDone,
		FilesFound:    t.filesFound,
		DirsFound:     t.dirsFound,
		ThroughputBPS: t.throughput,
		ElapsedSec:    now.Sub(t.started).Seconds(),
		ETASeconds:    ETAUnknown,
	}

	// ETA comes from bytes, never file counts: counts mislead badly on a
	// tree of mixed sizes (SPEC.md §6.1.1).
	if t.phase == PhaseCopying && t.throughput > 0 && t.bytesTotal > 0 {
		remaining := t.bytesTotal - t.bytesDone
		if remaining < 0 {
			remaining = 0
		}
		s.ETASeconds = float64(remaining) / t.throughput
	}

	for relPath, f := range t.inFlight {
		item := FileInFlight{RelPath: relPath, BytesDone: f.done, BytesTotal: f.total, ETASeconds: ETAUnknown}
		if f.total > 0 {
			item.Percent = float64(f.done) / float64(f.total) * 100
		}

		// Per-file rate from this file's own elapsed time (SPEC.md §6.1.1).
		if elapsed := now.Sub(f.started).Seconds(); elapsed > 0 && f.done > 0 {
			item.ThroughputBPS = float64(f.done) / elapsed
		}

		// Prefer the file's own rate; fall back to the run's while the file
		// is too young to have one.
		rate := item.ThroughputBPS
		if rate == 0 {
			rate = t.throughput
		}
		if rate > 0 && f.total > f.done {
			item.ETASeconds = float64(f.total-f.done) / rate
		}
		s.InFlight = append(s.InFlight, item)
	}
	sort.Slice(s.InFlight, func(i, j int) bool { return s.InFlight[i].RelPath < s.InFlight[j].RelPath })

	return s
}
