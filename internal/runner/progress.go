package runner

import (
	"sync"
	"time"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// DestSnapshot is one destination's progress within a run.
type DestSnapshot struct {
	DestTargetID string           `json:"dest_target_id"`
	Status       store.DestStatus `json:"status"`
	engine.Snapshot
}

// RunSnapshot is the whole run: the aggregate plus each destination
// (SPEC.md §6.1.1).
type RunSnapshot struct {
	Phase engine.Phase `json:"phase"`

	FilesTotal   int64 `json:"files_total"`
	FilesDone    int64 `json:"files_done"`
	FilesDeleted int64 `json:"files_deleted"`
	FilesFailed  int64 `json:"files_failed"`
	BytesTotal   int64 `json:"bytes_total"`
	BytesDone    int64 `json:"bytes_done"`

	ThroughputBPS float64 `json:"throughput_bps"`
	ETASeconds    float64 `json:"eta_sec"`
	ElapsedSec    float64 `json:"elapsed_sec"`

	// PendingDestinations have not been planned yet, so their real size is
	// unknown. EstimatedPendingBytes is the mean of the planned
	// destinations applied to them, which is what lets the total ETA
	// account for work not yet started. It is an estimate and is reported
	// separately so nothing pretends otherwise.
	PendingDestinations   int   `json:"pending_destinations"`
	EstimatedPendingBytes int64 `json:"estimated_pending_bytes"`

	Destinations []DestSnapshot        `json:"destinations"`
	InFlight     []engine.FileInFlight `json:"in_flight"`
}

// progress aggregates one tracker per destination.
type progress struct {
	mu sync.Mutex

	started  time.Time
	phase    engine.Phase
	order    []string
	trackers map[string]*engine.Tracker
	statuses map[string]store.DestStatus
	// planned marks destinations whose totals are known.
	planned map[string]bool
}

// phaseRank orders phases by how far through the pipeline they are, so the
// aggregate reports the most advanced work any destination is doing.
var phaseRank = map[engine.Phase]int{
	engine.PhaseScanning: 0,
	engine.PhasePlanning: 1,
	engine.PhaseCopying:  2,
	engine.PhaseDeleting: 3,
}

func busiestPhase(runPhase engine.Phase, dests []DestSnapshot) engine.Phase {
	best := runPhase
	for _, d := range dests {
		if d.Status != store.DestRunning {
			continue
		}
		if phaseRank[d.Phase] > phaseRank[best] {
			best = d.Phase
		}
	}
	return best
}

func newProgress(now time.Time, destTargetIDs []string) *progress {
	p := &progress{
		started:  now,
		phase:    engine.PhaseScanning,
		order:    append([]string{}, destTargetIDs...),
		trackers: map[string]*engine.Tracker{},
		statuses: map[string]store.DestStatus{},
		planned:  map[string]bool{},
	}
	for _, id := range destTargetIDs {
		p.trackers[id] = engine.NewTracker(now)
		p.statuses[id] = store.DestPending
	}
	return p
}

// tracker returns one destination's tracker.
func (p *progress) tracker(destTargetID string) *engine.Tracker {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.trackers[destTargetID]
}

// setPhase records what the run as a whole is doing.
func (p *progress) setPhase(phase engine.Phase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
}

// setStatus records a destination's outcome.
func (p *progress) setStatus(destTargetID string, status store.DestStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses[destTargetID] = status
}

// markPlanned notes that a destination's totals are now known.
func (p *progress) markPlanned(destTargetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.planned[destTargetID] = true
}

// setScanProgress mirrors the shared source scan onto every destination, so a
// caller polling any of them sees the discovery counts.
func (p *progress) setScanProgress(files, dirs int64) {
	p.mu.Lock()
	trackers := make([]*engine.Tracker, 0, len(p.trackers))
	for _, t := range p.trackers {
		trackers = append(trackers, t)
	}
	p.mu.Unlock()

	for _, t := range trackers {
		t.SetScanProgress(files, dirs)
	}
}

// sample folds elapsed bytes into every destination's rolling throughput.
func (p *progress) sample(now time.Time) {
	p.mu.Lock()
	trackers := make([]*engine.Tracker, 0, len(p.trackers))
	for _, t := range p.trackers {
		trackers = append(trackers, t)
	}
	p.mu.Unlock()

	for _, t := range trackers {
		t.Sample(now)
	}
}

// snapshot builds the aggregate view.
func (p *progress) snapshot(now time.Time) RunSnapshot {
	p.mu.Lock()
	order := append([]string{}, p.order...)
	trackers := make(map[string]*engine.Tracker, len(p.trackers))
	statuses := make(map[string]store.DestStatus, len(p.statuses))
	planned := make(map[string]bool, len(p.planned))
	for k, v := range p.trackers {
		trackers[k] = v
	}
	for k, v := range p.statuses {
		statuses[k] = v
	}
	for k, v := range p.planned {
		planned[k] = v
	}
	phase := p.phase
	started := p.started
	p.mu.Unlock()

	out := RunSnapshot{Phase: phase, ElapsedSec: now.Sub(started).Seconds(), ETASeconds: engine.ETAUnknown}

	var plannedCount int
	for _, id := range order {
		snap := trackers[id].Snapshot(now)

		out.FilesTotal += snap.FilesTotal
		out.FilesDone += snap.FilesDone
		out.FilesDeleted += snap.FilesDeleted
		out.FilesFailed += snap.FilesFailed
		out.BytesTotal += snap.BytesTotal
		out.BytesDone += snap.BytesDone
		out.ThroughputBPS += snap.ThroughputBPS
		out.InFlight = append(out.InFlight, snap.InFlight...)

		if planned[id] {
			plannedCount++
		} else if statuses[id] == store.DestPending || statuses[id] == store.DestRunning {
			out.PendingDestinations++
		}

		out.Destinations = append(out.Destinations, DestSnapshot{
			DestTargetID: id,
			Status:       statuses[id],
			Snapshot:     snap,
		})
	}

	// The run-level phase is whatever the destinations are actually doing.
	// Setting it only at the run level would report "scanning" for the whole
	// copy, since the shared source scan is the last thing the run itself
	// does before handing off to each destination.
	if phase != engine.PhaseDone {
		out.Phase = busiestPhase(phase, out.Destinations)
	}

	// SPEC.md §6.1.1 wants the total to account for destinations that have
	// not started. Their real size is unknown until they are planned, so
	// they are estimated from the mean of the ones that are.
	if out.PendingDestinations > 0 && plannedCount > 0 {
		out.EstimatedPendingBytes = (out.BytesTotal / int64(plannedCount)) * int64(out.PendingDestinations)
	}

	if out.ThroughputBPS > 0 {
		remaining := out.BytesTotal + out.EstimatedPendingBytes - out.BytesDone
		if remaining < 0 {
			remaining = 0
		}
		out.ETASeconds = float64(remaining) / out.ThroughputBPS
	}
	return out
}
