package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/filter"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// DefaultFilterSampleLimit caps how much of a preview is returned.
const DefaultFilterSampleLimit = 100

// maxFilterScan bounds how much of the tree a preview walks. A filter test is
// a UI button, not a run: it must answer quickly even against a share holding
// a million files.
const maxFilterScan = 20000

// FilterSample is one path and what the chain decided about it.
type FilterSample struct {
	RelPath string `json:"relpath"`
	IsDir   bool   `json:"is_dir"`
	// Rule names the rule responsible, empty when nothing matched.
	Rule string `json:"rule,omitempty"`
}

// FilterTestDest is the preview for one destination, since per-target rules
// mean two destinations of one job can see different subsets.
type FilterTestDest struct {
	DestTargetID  string          `json:"dest_target_id"`
	Included      []FilterSample  `json:"included"`
	Excluded      []FilterSample  `json:"excluded"`
	IncludedTotal int             `json:"included_total"`
	ExcludedTotal int             `json:"excluded_total"`
	Rules         []filter.Counts `json:"rules"`
}

// FilterTestResult is what POST /api/jobs/{id}/filter-test returns.
type FilterTestResult struct {
	ScannedPaths int              `json:"scanned_paths"`
	Truncated    bool             `json:"truncated"`
	Destinations []FilterTestDest `json:"destinations"`
}

// FilterTest previews a job's filters against a sample of its real source
// (SPEC.md §6.5, §8).
// ErrSourceUnavailable reports that a filter test could not reach the source,
// as opposed to failing on the rules themselves.
var ErrSourceUnavailable = errors.New("the source is unavailable")

func (r *Runner) FilterTest(ctx context.Context, job *store.Job, sampleLimit int) (*FilterTestResult, error) {
	if sampleLimit <= 0 || sampleLimit > 1000 {
		sampleLimit = DefaultFilterSampleLimit
	}

	// Warnings are discarded here: a preview reports problems through its
	// error, not through a run log that does not exist.
	discard := newEventBuffer(ctx, r.db, "", r.log)
	chains, _, err := r.resolveChains(ctx, job, discard)
	if err != nil {
		return nil, err
	}

	srcTarget, err := r.db.GetTarget(ctx, job.SourceTargetID)
	if err != nil {
		return nil, fmt.Errorf("loading the source target: %w", err)
	}
	srcRoot, _, release, err := r.resolve(ctx, srcTarget, job.SourceSubpath)
	if err != nil {
		// Wrapped in a sentinel so the API can answer 502 rather than 400: a
		// share being down is not a malformed request, and telling the user
		// their filter rules are wrong when the NAS is unplugged sends them
		// looking in the wrong place.
		return nil, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	defer release()

	// The preview walks unpruned: a pruned directory would simply be absent,
	// and the whole point is to show what the rules exclude.
	scan, err := (&engine.Scanner{Limit: maxFilterScan}).Scan(ctx, srcRoot)
	if err != nil {
		return nil, fmt.Errorf("scanning the source: %w", err)
	}

	paths := make([]string, 0, len(scan.Entries))
	for relPath := range scan.Entries {
		paths = append(paths, relPath)
	}
	sort.Strings(paths)

	result := &FilterTestResult{ScannedPaths: len(paths), Truncated: scan.Truncated}

	for _, dest := range job.Destinations {
		chain := chains[dest.DestTargetID]
		out := FilterTestDest{DestTargetID: dest.DestTargetID, Included: []FilterSample{}, Excluded: []FilterSample{}}

		for _, relPath := range paths {
			entry := scan.Entries[relPath]
			admitted, rule := chain.Explain(relPath, entry.IsDir)

			sample := FilterSample{RelPath: relPath, IsDir: entry.IsDir}
			if rule != nil {
				sample.Rule = rule.Description
			}

			if admitted {
				out.IncludedTotal++
				if len(out.Included) < sampleLimit {
					out.Included = append(out.Included, sample)
				}
				continue
			}
			out.ExcludedTotal++
			if len(out.Excluded) < sampleLimit {
				out.Excluded = append(out.Excluded, sample)
			}
		}

		out.Rules = chain.Counts()
		result.Destinations = append(result.Destinations, out)
	}
	return result, nil
}
