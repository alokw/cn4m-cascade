package runner

import (
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// StatusPayload builds the status shape of SPEC.md §8.1.
//
// It lives here, in neither the API nor the notifier, because §8.2 says an
// outbound callback carries "the same status JSON as the polling endpoint plus
// an `event` field". Two builders would satisfy that on the day they were
// written and drift the first time a field was added to one of them — and this
// is the one shape consumed by software outside this project, where drift is a
// broken integration rather than a cosmetic bug.
//
// nameFor resolves a destination target id to a human name; a poller reading
// "nas-basement failed" is served far better than one reading a hex string.
func StatusPayload(run *store.Run, snap *RunSnapshot, nameFor func(string) string) map[string]any {
	out := map[string]any{
		"run_id":     run.ID,
		"job_id":     run.JobID,
		"status":     string(run.Status),
		"trigger":    string(run.Trigger),
		"started_at": run.StartedAt,
	}
	// FinishedAt is a *time.Time and is nil while the run is going — the whole
	// point of this payload is describing runs that have not finished, so this
	// is the common case rather than an edge one.
	if run.FinishedAt != nil {
		out["finished_at"] = *run.FinishedAt
		out["elapsed_sec"] = int(run.FinishedAt.Sub(run.StartedAt).Seconds())
	} else {
		out["elapsed_sec"] = int(time.Since(run.StartedAt).Seconds())
	}
	if run.ErrorSummary != "" {
		out["last_error"] = run.ErrorSummary
	}

	if snap != nil {
		out["phase"] = string(snap.Phase)
		out["files_done"] = snap.FilesDone
		out["files_total"] = snap.FilesTotal
		out["bytes_done"] = snap.BytesDone
		out["bytes_total"] = snap.BytesTotal
		out["throughput_bps"] = snap.ThroughputBPS
		// The size of the source tree, which is a different question from how
		// much is being transferred. Reported in both states so a caller does
		// not have to wait for a run to finish to learn how big the job is.
		out["scanned_files"] = snap.ScannedFiles
		out["scanned_bytes"] = snap.ScannedBytes
		out["eta_sec"] = snap.ETASeconds

		files := make([]map[string]any, 0, len(snap.InFlight))
		for _, f := range snap.InFlight {
			files = append(files, map[string]any{
				"relpath": f.RelPath, "pct": f.Percent, "eta_sec": f.ETASeconds,
			})
		}
		out["current_files"] = files
	} else {
		// A finished run's counters come from the flushed per-destination rows,
		// summed — not from the run row's scan totals.
		//
		// **files_total and bytes_total mean the transfer, in both states.**
		// They used to switch meaning the moment a run ended: the transfer
		// while running, the size of the source tree afterwards, so a job that
		// copied 5 MB out of a 1 GB tree reported 1 GB once it finished. The
		// tree is still reported, as scanned_files/scanned_bytes, because it
		// answers a genuine question — just not this one.
		var filesTotal, filesDone, bytesTotal, bytesDone int64
		for _, d := range run.Destinations {
			filesTotal += d.FilesTotal
			filesDone += d.FilesDone
			bytesTotal += d.BytesTotal
			bytesDone += d.BytesDone
		}
		out["files_done"] = filesDone
		out["files_total"] = filesTotal
		out["bytes_done"] = bytesDone
		out["bytes_total"] = bytesTotal
		out["current_files"] = []map[string]any{}
		// Present but meaningless rather than absent. §11 requires the
		// documented shape "both during a run and after it", and a poller that
		// has to branch on whether a key exists is the thing a fixed shape is
		// supposed to prevent. -1 is the project's existing "not computable"
		// value (engine.ETAUnknown), not zero, which would read as "finishing
		// right now".
		out["scanned_files"] = run.FilesScanned
		out["scanned_bytes"] = run.BytesTotal
		out["throughput_bps"] = 0
		out["eta_sec"] = -1
	}

	// Live per-destination counters, keyed by target, so the destinations block
	// does not lag the aggregate above it. The flushed rows update about once
	// a second; without this a poller sees a run 80% done overall whose
	// destinations still add up to 60%.
	live := map[string]DestSnapshot{}
	if snap != nil {
		for _, d := range snap.Destinations {
			live[d.DestTargetID] = d
		}
	}

	dests := make([]map[string]any, 0, len(run.Destinations))
	errorsCount := 0
	for _, d := range run.Destinations {
		name := d.DestTargetID
		if nameFor != nil {
			name = nameFor(d.DestTargetID)
		}
		entry := map[string]any{
			"target":      name,
			"status":      string(d.Status),
			"files_done":  d.FilesDone,
			"files_total": d.FilesTotal,
			"bytes_done":  d.BytesDone,
			"bytes_total": d.BytesTotal,
			// SPEC.md §8.1 lists eta_sec per destination. -1 until the engine
			// can compute one, which is what engine.ETAUnknown means.
			"eta_sec": -1.0,
		}
		if ls, ok := live[d.DestTargetID]; ok {
			entry["status"] = string(ls.Status)
			entry["files_done"] = ls.FilesDone
			entry["files_total"] = ls.FilesTotal
			entry["bytes_done"] = ls.BytesDone
			entry["bytes_total"] = ls.BytesTotal
			entry["eta_sec"] = ls.ETASeconds
		}
		dests = append(dests, entry)
		if d.ErrorSummary != "" {
			errorsCount++
		}
	}
	out["destinations"] = dests
	out["errors_count"] = errorsCount
	return out
}
