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
	// PromptDeadline is when an unanswered prompt for this destination
	// falls back (SPEC.md §9's visible countdown). Zero unless Status is
	// awaiting_prompt.
	PromptDeadline time.Time `json:"prompt_deadline,omitempty"`
	// PromptReason is why the destination is being asked about.
	PromptReason string `json:"prompt_reason,omitempty"`
	// PromptCanCreate means the destination resolved but its folder does not
	// exist, so creating it is one of the answers.
	PromptCanCreate bool `json:"prompt_can_create,omitempty"`
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

	// ScannedFiles and ScannedBytes are how much the source scan found in
	// scope — the size of the tree, not the size of the transfer.
	//
	// They exist so an operator can anticipate the shape of a job while it is
	// still running; until now the scan size was only visible once the run had
	// finished. They are deliberately **not** the progress denominator: a tree
	// that is already in sync is large and has nothing to copy, and confusing
	// the two is what made a 700 MB transfer report 3.5 TB (D-130).
	ScannedFiles int64 `json:"scanned_files"`
	ScannedBytes int64 `json:"scanned_bytes"`

	// ConfirmDeadline is when a previewed run gives up waiting and cancels
	// itself. Zero unless the run is awaiting confirmation.
	ConfirmDeadline time.Time `json:"confirm_deadline,omitempty"`

	Destinations []DestSnapshot        `json:"destinations"`
	InFlight     []engine.FileInFlight `json:"in_flight"`
	// Plans is the previewed work, present once planning is done. In a
	// preview run this is what the user is being asked to confirm.
	Plans []DestPlan `json:"plans,omitempty"`
}

// DestPlan is what a run intends to do to one destination (SPEC.md §6.1
// step 6). It is a snapshot: a confirmed preview executes exactly this, not a
// freshly recomputed diff, because this is what the user agreed to.
type DestPlan struct {
	DestTargetID string `json:"dest_target_id"`

	MkDirs    int   `json:"mkdirs"`
	Copies    int   `json:"copies"`
	Deletes   int   `json:"deletes"`
	RmDirs    int   `json:"rmdirs"`
	CopyBytes int64 `json:"copy_bytes"`

	// Replaces counts removals that clear something of the wrong type out of
	// the way. Deletes deliberately excludes them, so without this count the
	// one list the UI shows would have no total to check itself against —
	// which is how a truncated list of destructive actions gets approved.
	Replaces int `json:"replaces"`
	// Overwrites counts copies that replace an existing destination file.
	Overwrites int `json:"overwrites"`

	// DeletionsBlocked reports that deletions were planned and withheld;
	// the UI must show this, because it is the difference between "nothing
	// to delete" and "refusing to delete".
	DeletionsBlocked bool   `json:"deletions_blocked,omitempty"`
	BlockedReason    string `json:"blocked_reason,omitempty"`
	// WithheldDeletes and WithheldRmDirs say *how many* were withheld, so the
	// refusal can be stated at its real scale rather than as a bare flag.
	WithheldDeletes int      `json:"withheld_deletes,omitempty"`
	WithheldRmDirs  int      `json:"withheld_rmdirs,omitempty"`
	Conflicts       []string `json:"conflicts,omitempty"`
}

// planPathLimit caps each list in DestActions. A mirror of a large tree can
// plan hundreds of thousands of removals, and a confirm screen that tries to
// render all of them helps nobody; the untruncated counts live on DestPlan.
// Matches the convention of storage.listLimit and FilterTestResult's samples.
const planPathLimit = 2000

// PlannedAction is one path a run intends to act on. Deletions are listed
// individually because CLAUDE.md forbids summarising them: a user confirming
// "412 files" is entitled to see which 412.
type PlannedAction struct {
	RelPath string `json:"relpath"`
	Size    int64  `json:"size,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Overwrite marks a copy that replaces an existing file rather than
	// creating a new one.
	Overwrite bool `json:"overwrite,omitempty"`
}

// DestActions is the per-path detail behind a DestPlan's counts, served by
// GET /api/runs/{id}/plan rather than ridden along on every progress frame:
// plans are broadcast to every WebSocket client once a second, and path lists
// have no business in that traffic.
type DestActions struct {
	DestTargetID string `json:"dest_target_id"`

	MkDirs []string        `json:"mkdirs"`
	Copies []PlannedAction `json:"copies"`
	// Deletes is the trailing delete pass only.
	Deletes []PlannedAction `json:"deletes"`
	RmDirs  []string        `json:"rmdirs"`
	// Replaces are removals that clear something of the wrong type out of the
	// way of a copy or mkdir (engine.Action.Unblock). They destroy data too,
	// so they are shown — but they are *not* folded into Deletes, whose count
	// on DestPlan deliberately excludes them. Conflating the two would show a
	// user more rows than the number printed beside them.
	Replaces []PlannedAction `json:"replaces"`

	// Truncated reports that at least one list above was capped at
	// planPathLimit. The true totals are on the matching DestPlan.
	Truncated bool `json:"truncated"`
}

// actionsOf projects an engine plan onto the per-path view.
func actionsOf(destID string, plan *engine.Plan) DestActions {
	da := DestActions{
		DestTargetID: destID,
		MkDirs:       []string{},
		Copies:       []PlannedAction{},
		Deletes:      []PlannedAction{},
		RmDirs:       []string{},
		Replaces:     []PlannedAction{},
	}
	if plan == nil {
		return da
	}

	for _, a := range plan.Actions {
		switch {
		// Unblock first, mirroring engine's partition. Both ActionDelete and
		// ActionRmDir can carry it today; checking Kind first would silently
		// file a future unblocked mkdir or copy under "will be created",
		// hiding a removal.
		case a.Unblock:
			if len(da.Replaces) < planPathLimit {
				da.Replaces = append(da.Replaces, PlannedAction{
					RelPath: a.RelPath, Size: a.Size, Reason: a.Reason})
			} else {
				da.Truncated = true
			}
		case a.Kind == engine.ActionMkDir:
			if len(da.MkDirs) < planPathLimit {
				da.MkDirs = append(da.MkDirs, a.RelPath)
			} else {
				da.Truncated = true
			}
		case a.Kind == engine.ActionCopy:
			if len(da.Copies) < planPathLimit {
				da.Copies = append(da.Copies, PlannedAction{
					RelPath: a.RelPath, Size: a.Size, Reason: a.Reason, Overwrite: a.Overwrite})
			} else {
				da.Truncated = true
			}
		case a.Kind == engine.ActionDelete:
			if len(da.Deletes) < planPathLimit {
				da.Deletes = append(da.Deletes, PlannedAction{
					RelPath: a.RelPath, Size: a.Size, Reason: a.Reason})
			} else {
				da.Truncated = true
			}
		case a.Kind == engine.ActionRmDir:
			if len(da.RmDirs) < planPathLimit {
				da.RmDirs = append(da.RmDirs, a.RelPath)
			} else {
				da.Truncated = true
			}
		}
	}
	return da
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

	// promptDeadlines and promptReasons drive the countdown the UI shows
	// while a destination waits for an answer.
	promptDeadlines map[string]time.Time
	promptReasons   map[string]string
	// promptCanCreate marks the prompts where the folder is merely absent, so
	// the UI can offer Create. Without it the modal would have to infer the
	// difference from the message text.
	promptCanCreate map[string]bool
	// confirmDeadline is when an unconfirmed preview cancels itself.
	confirmDeadline time.Time
	// plans is what each destination intends to do, once diffed.
	plans map[string]DestPlan

	// scannedFiles and scannedBytes are what the shared source scan found.
	// Reported for context only; see RunSnapshot.ScannedBytes.
	scannedFiles int64
	scannedBytes int64
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
		started:         now,
		phase:           engine.PhaseScanning,
		order:           append([]string{}, destTargetIDs...),
		trackers:        map[string]*engine.Tracker{},
		statuses:        map[string]store.DestStatus{},
		planned:         map[string]bool{},
		promptDeadlines: map[string]time.Time{},
		promptReasons:   map[string]string{},
		promptCanCreate: map[string]bool{},
		plans:           map[string]DestPlan{},
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

// setPrompt records that a destination is waiting for an answer, and when
// that wait runs out.
func (p *progress) setPrompt(destTargetID string, deadline time.Time, reason string, canCreate bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.promptDeadlines[destTargetID] = deadline
	p.promptReasons[destTargetID] = reason
	if canCreate {
		p.promptCanCreate[destTargetID] = true
	} else {
		delete(p.promptCanCreate, destTargetID)
	}
}

// clearPrompt records that a destination is no longer waiting.
func (p *progress) clearPrompt(destTargetID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.promptDeadlines, destTargetID)
	delete(p.promptReasons, destTargetID)
	delete(p.promptCanCreate, destTargetID)
}

// setConfirmDeadline records when an unconfirmed preview gives up.
func (p *progress) setConfirmDeadline(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.confirmDeadline = at
}

// setPlan records what a destination intends to do.
func (p *progress) setPlan(destTargetID string, plan DestPlan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plans[destTargetID] = plan
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
// setSourceScan records the size of the source tree once the scan has finished.
//
// Informational. It must never reach FilesTotal or BytesTotal: those describe
// the transfer, and this describes the tree the transfer was chosen from.
func (p *progress) setSourceScan(files, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scannedFiles, p.scannedBytes = files, bytes
}

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
	promptDeadlines := make(map[string]time.Time, len(p.promptDeadlines))
	promptReasons := make(map[string]string, len(p.promptReasons))
	plans := make(map[string]DestPlan, len(p.plans))
	for k, v := range p.trackers {
		trackers[k] = v
	}
	for k, v := range p.statuses {
		statuses[k] = v
	}
	for k, v := range p.planned {
		planned[k] = v
	}
	for k, v := range p.promptDeadlines {
		promptDeadlines[k] = v
	}
	for k, v := range p.promptReasons {
		promptReasons[k] = v
	}
	promptCanCreate := make(map[string]bool, len(p.promptCanCreate))
	for k, v := range p.promptCanCreate {
		promptCanCreate[k] = v
	}
	for k, v := range p.plans {
		plans[k] = v
	}
	phase := p.phase
	scannedFiles, scannedBytes := p.scannedFiles, p.scannedBytes
	started := p.started
	confirmDeadline := p.confirmDeadline
	p.mu.Unlock()

	out := RunSnapshot{
		Phase:           phase,
		ElapsedSec:      now.Sub(started).Seconds(),
		ETASeconds:      engine.ETAUnknown,
		ScannedFiles:    scannedFiles,
		ScannedBytes:    scannedBytes,
		ConfirmDeadline: confirmDeadline,
	}

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
			DestTargetID:    id,
			Status:          statuses[id],
			PromptDeadline:  promptDeadlines[id],
			PromptReason:    promptReasons[id],
			PromptCanCreate: promptCanCreate[id],
			Snapshot:        snap,
		})
		if plan, ok := plans[id]; ok {
			out.Plans = append(out.Plans, plan)
		}
	}

	// The run-level phase is whatever the destinations are actually doing.
	// Setting it only at the run level would report "scanning" for the whole
	// copy, since the shared source scan is the last thing the run itself
	// does before handing off to each destination.
	if phase != engine.PhaseDone {
		out.Phase = busiestPhase(phase, out.Destinations)
	}

	// SPEC.md §6.1.1 wants the total to account for destinations that have not
	// started. They are **estimated from the mean of the ones that have**, and
	// folded into the totals rather than reported separately, so a fan-out run
	// stops counting upwards as each destination is planned.
	//
	// The estimate is of *work*, never of scope. An earlier version used the
	// source scan — files × destinations, fixed the moment the scan ended — and
	// it was wrong in a way that looked right: a 500 GB tree already in sync
	// reported 3.5 TB to copy when the actual work was 700 MB, because every
	// file in scope counted whether or not it needed copying. A progress bar
	// and an ETA describe what is going to be transferred, so the denominator
	// has to be the transfer.
	if out.PendingDestinations > 0 && plannedCount > 0 {
		pending := int64(out.PendingDestinations)
		out.EstimatedPendingBytes = (out.BytesTotal / int64(plannedCount)) * pending
		out.FilesTotal += (out.FilesTotal / int64(plannedCount)) * pending
		out.BytesTotal += out.EstimatedPendingBytes
	}

	if out.ThroughputBPS > 0 {
		// EstimatedPendingBytes is already inside BytesTotal; adding it again
		// here would double count every destination not yet planned.
		remaining := out.BytesTotal - out.BytesDone
		if remaining < 0 {
			remaining = 0
		}
		out.ETASeconds = float64(remaining) / out.ThroughputBPS
	}
	return out
}
