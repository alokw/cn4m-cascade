package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// ActionKind is one step in a plan.
type ActionKind string

const (
	ActionMkDir  ActionKind = "mkdir"
	ActionCopy   ActionKind = "copy"
	ActionDelete ActionKind = "delete"
	ActionRmDir  ActionKind = "rmdir"
)

// Action is a single unit of work against one destination.
type Action struct {
	Kind    ActionKind
	RelPath string
	Size    int64
	ModTime time.Time
	// Reason is why this action exists, for the run log.
	Reason string
	// Unblock marks a removal that has to happen *before* the create stage
	// rather than in the trailing delete pass: something of the wrong type
	// is sitting where a directory or file needs to go, and every mkdir or
	// copy beneath it fails until it is cleared.
	Unblock bool
	// Overwrite marks a copy that replaces an existing destination file, as
	// opposed to creating a new one. Overwrites are always logged
	// individually; new files are only logged when the job asks for it.
	Overwrite bool
}

// Conflict is something the plan deliberately declined to do, and why. These
// surface in the run log so a skipped file is never silent.
type Conflict struct {
	RelPath string
	Reason  string
}

// Plan is the ordered work for one destination, with the byte totals that
// drive progress and ETA (SPEC.md §6.1 step 5).
type Plan struct {
	Actions []Action

	MkDirs    int
	Copies    int
	Deletes   int
	RmDirs    int
	CopyBytes int64

	// Conflicts are paths skipped rather than acted on.
	Conflicts []Conflict

	// DeletionsBlocked is set when deletions were planned but withheld.
	DeletionsBlocked bool
	BlockedReason    string
}

// DiffOptions are the comparison rules from SPEC.md §6.2.
type DiffOptions struct {
	Mode      store.SyncMode
	Tolerance time.Duration
	// IgnoreDSTHour treats a whole-hour offset as equal, the classic
	// FAT/DST artefact FreeFileSync also has a toggle for.
	IgnoreDSTHour bool
	// CaseInsensitiveDest tells the differ that two destination paths
	// differing only in case are the same file. SMB targets usually are
	// (SPEC.md §6.5).
	CaseInsensitiveDest bool
}

// Diff produces the ordered action plan that makes dst match src under the
// job's mode.
//
// Ordering is fixed here rather than left to the executor: anything blocking
// a path is cleared first, then directories are created top-down, then
// copies, then removals bottom-up so a directory only goes once emptied
// (SPEC.md §6.1 step 7).
func Diff(src, dst *ScanResult, opts DiffOptions) *Plan {
	plan := &Plan{}
	mirroring := opts.Mode == store.ModeMirror

	var (
		unblocks []Action
		mkdirs   []Action
		copies   []Action
		deletes  []Action
		rmdirs   []Action
	)

	// Two source paths that differ only in case collide at a
	// case-insensitive destination: both would be written to one name, and
	// which one survives is a race. Neither is copied.
	skipSource := map[string]bool{}
	if opts.CaseInsensitiveDest {
		for _, group := range foldedGroups(src.Entries) {
			if len(group) < 2 {
				continue
			}
			sort.Strings(group)
			for _, relPath := range group[1:] {
				skipSource[relPath] = true
			}
			plan.Conflicts = append(plan.Conflicts, Conflict{
				RelPath: group[0],
				Reason: fmt.Sprintf(
					"skipped: %s differ only in case and would collide at a case-insensitive destination",
					strings.Join(group, ", ")),
			})
		}
	}

	for _, relPath := range sortedKeys(src.Entries) {
		if skipSource[relPath] {
			continue
		}
		srcEntry := src.Entries[relPath]
		dstEntry, exists := dst.Entries[relPath]

		// A symlink at the source is never synced (SPEC.md §13).
		if srcEntry.IsSymlink {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				RelPath: relPath,
				Reason:  "skipped: symlinks are not synced in this version",
			})
			continue
		}

		// A symlink at the destination is in the way of whatever the source
		// has here. Writing through it would land outside the destination
		// root, so it is cleared first, or reported if we may not delete.
		if exists && dstEntry.IsSymlink {
			if !mirroring {
				plan.Conflicts = append(plan.Conflicts, Conflict{
					RelPath: relPath,
					Reason:  "skipped: a symlink is at this path, and update mode never deletes",
				})
				continue
			}
			unblocks = append(unblocks, Action{
				Kind: ActionDelete, RelPath: relPath, Unblock: true,
				Reason: "a symlink is where the source has a real file or directory",
			})
			if srcEntry.IsDir {
				mkdirs = append(mkdirs, Action{
					Kind: ActionMkDir, RelPath: relPath, Reason: "replacing a symlink with a directory",
				})
			} else {
				copies = append(copies, copyAction(srcEntry, "replacing a symlink with a file"))
			}
			continue
		}

		if srcEntry.IsDir {
			switch {
			case !exists:
				mkdirs = append(mkdirs, Action{
					Kind: ActionMkDir, RelPath: relPath, Reason: "missing at the destination",
				})

			case !dstEntry.IsDir && mirroring:
				// A file is where a directory belongs. It has to go before
				// the directory can be created, so it is an unblock rather
				// than part of the trailing delete pass.
				unblocks = append(unblocks, Action{
					Kind: ActionDelete, RelPath: relPath, Size: dstEntry.Size, Unblock: true,
					Reason: "a file is where the source has a directory",
				})
				mkdirs = append(mkdirs, Action{
					Kind: ActionMkDir, RelPath: relPath, Reason: "replacing a file with a directory",
				})

			case !dstEntry.IsDir:
				// Update mode never deletes, so the conflict is reported and
				// the subtree left alone (SPEC.md §1).
				plan.Conflicts = append(plan.Conflicts, Conflict{
					RelPath: relPath,
					Reason:  "skipped: a file is where the source has a directory, and update mode never deletes",
				})
			}
			continue
		}

		switch {
		case !exists:
			copies = append(copies, copyAction(srcEntry, "new at the source"))

		case dstEntry.IsDir && mirroring:
			// A directory is where a file belongs; it must go before the
			// file can be renamed into place.
			unblocks = append(unblocks, Action{
				Kind: ActionRmDir, RelPath: relPath, Unblock: true,
				Reason: "a directory is where the source has a file",
			})
			copies = append(copies, copyAction(srcEntry, "replacing a directory with a file"))

		case dstEntry.IsDir:
			plan.Conflicts = append(plan.Conflicts, Conflict{
				RelPath: relPath,
				Reason:  "skipped: a directory is where the source has a file, and update mode never deletes",
			})

		case sameFile(srcEntry, dstEntry, opts):
			// Nothing to do — the case an idempotent re-run must hit.

		case !mirroring && !srcIsNewer(srcEntry, dstEntry, opts):
			// Update copies new and newer files only; a destination that is
			// the same age or newer is left as it is (SPEC.md §1).
			plan.Conflicts = append(plan.Conflicts, Conflict{
				RelPath: relPath,
				Reason:  "skipped: the destination copy is not older, and update mode only copies newer files",
			})

		default:
			action := copyAction(srcEntry, differenceReason(srcEntry, dstEntry))
			action.Overwrite = true
			copies = append(copies, action)
		}
	}

	// Mirror also removes whatever the source no longer has (SPEC.md §1).
	if mirroring {
		// A destination path that matches a source path apart from case is
		// the same file on a case-insensitive server. Deleting it would
		// delete the copy that was just written to it.
		foldedSrc := map[string]bool{}
		if opts.CaseInsensitiveDest {
			for relPath := range src.Entries {
				foldedSrc[strings.ToLower(relPath)] = true
			}
		}

		for _, relPath := range sortedKeys(dst.Entries) {
			if _, exists := src.Entries[relPath]; exists {
				continue
			}
			if opts.CaseInsensitiveDest && foldedSrc[strings.ToLower(relPath)] {
				plan.Conflicts = append(plan.Conflicts, Conflict{
					RelPath: relPath,
					Reason:  "not deleted: the source has this path under different capitalisation",
				})
				continue
			}

			if dst.Entries[relPath].IsDir {
				rmdirs = append(rmdirs, Action{
					Kind: ActionRmDir, RelPath: relPath, Reason: "not present at the source",
				})
			} else {
				deletes = append(deletes, Action{
					Kind: ActionDelete, RelPath: relPath, Size: dst.Entries[relPath].Size,
					Reason: "not present at the source",
				})
			}
		}

		if reason := deletionGuard(src, dst); reason != "" {
			plan.DeletionsBlocked = len(deletes)+len(rmdirs) > 0
			plan.BlockedReason = reason
			deletes, rmdirs = nil, nil
		}
	}

	// Shallowest first, so parents exist before their children.
	sortByDepth(mkdirs, true)
	// Deepest first, so a directory is empty by the time it is removed.
	sortByDepth(unblocks, false)
	sortByDepth(deletes, false)
	sortByDepth(rmdirs, false)
	sort.Slice(copies, func(i, j int) bool { return copies[i].RelPath < copies[j].RelPath })

	plan.MkDirs = len(mkdirs)
	plan.Copies = len(copies)
	plan.Deletes = len(deletes)
	plan.RmDirs = len(rmdirs)
	for _, a := range copies {
		plan.CopyBytes += a.Size
	}

	plan.Actions = make([]Action, 0, len(unblocks)+len(mkdirs)+len(copies)+len(deletes)+len(rmdirs))
	plan.Actions = append(plan.Actions, unblocks...)
	plan.Actions = append(plan.Actions, mkdirs...)
	plan.Actions = append(plan.Actions, copies...)
	plan.Actions = append(plan.Actions, deletes...)
	plan.Actions = append(plan.Actions, rmdirs...)
	return plan
}

// deletionGuard returns a reason to withhold every deletion, or "".
//
// Both cases it catches are ways a mirror destroys data on the strength of a
// source listing that does not reflect reality.
func deletionGuard(src, dst *ScanResult) string {
	// An unreadable source directory makes everything beneath it look
	// extraneous at the destination.
	if src.Incomplete() {
		return fmt.Sprintf(
			"the source could not be fully read (%d path%s unreadable), so nothing was deleted",
			len(src.Errors), plural(len(src.Errors)))
	}

	// A source that lists as completely empty while the destination is not
	// is far more likely to be a dropped mount or a stale cached listing
	// than a genuine request to delete everything (SPEC.md §13 warns that
	// cache=loose can serve a stale listing).
	if len(src.Entries) == 0 && len(dst.Entries) > 0 {
		return fmt.Sprintf(
			"the source scanned as completely empty while the destination holds %d path(s), "+
				"which usually means the source is not really mounted; nothing was deleted",
			len(dst.Entries))
	}
	return ""
}

// Empty reports whether the plan would change nothing — what an idempotent
// re-run of an already-synced tree must produce.
func (p *Plan) Empty() bool { return len(p.Actions) == 0 }

func copyAction(e Entry, reason string) Action {
	return Action{Kind: ActionCopy, RelPath: e.RelPath, Size: e.Size, ModTime: e.ModTime, Reason: reason}
}

// sameFile applies SPEC.md §6.2: size plus mtime within a tolerance, because
// SMB and FAT mtime granularity is coarse.
func sameFile(src, dst Entry, opts DiffOptions) bool {
	if src.Size != dst.Size {
		return false
	}

	diff := src.ModTime.Sub(dst.ModTime)
	if diff < 0 {
		diff = -diff
	}
	if diff <= opts.Tolerance {
		return true
	}

	// The classic whole-hour offset from a DST change or a filesystem that
	// stores local time.
	if opts.IgnoreDSTHour {
		offBy := diff - time.Hour
		if offBy < 0 {
			offBy = -offBy
		}
		return offBy <= opts.Tolerance
	}
	return false
}

// srcIsNewer reports whether the source is meaningfully newer than the
// destination, which is what update mode copies on.
func srcIsNewer(src, dst Entry, opts DiffOptions) bool {
	return src.ModTime.After(dst.ModTime.Add(opts.Tolerance))
}

func differenceReason(src, dst Entry) string {
	if src.Size != dst.Size {
		return fmt.Sprintf("size differs (%d vs %d bytes)", src.Size, dst.Size)
	}
	return "modification time differs"
}

// foldedGroups buckets paths by their lowercased form, to find paths that
// collide on a case-insensitive filesystem.
func foldedGroups(entries map[string]Entry) map[string][]string {
	groups := map[string][]string{}
	for relPath := range entries {
		folded := strings.ToLower(relPath)
		groups[folded] = append(groups[folded], relPath)
	}
	return groups
}

func sortedKeys(entries map[string]Entry) []string {
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortByDepth orders actions by path depth, then lexically so the order is
// deterministic and reads sensibly in a log.
func sortByDepth(actions []Action, shallowestFirst bool) {
	sort.Slice(actions, func(i, j int) bool {
		di := strings.Count(actions[i].RelPath, "/")
		dj := strings.Count(actions[j].RelPath, "/")
		if di != dj {
			if shallowestFirst {
				return di < dj
			}
			return di > dj
		}
		return actions[i].RelPath < actions[j].RelPath
	})
}

func plural(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}
