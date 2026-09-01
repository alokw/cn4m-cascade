package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alokw/cn4m-cascade/internal/engine"
	"github.com/alokw/cn4m-cascade/internal/filter"
	"github.com/alokw/cn4m-cascade/internal/store"
)

// resolveChains builds one filter chain per destination.
//
// Rule files are read here, at the start of every run, because SPEC.md §6.5
// treats them as live configuration rather than something snapshotted when
// the job was saved. A rule that cannot be read is handled per its own
// on_error setting.
func (r *Runner) resolveChains(ctx context.Context, job *store.Job, events *eventBuffer) (map[string]*filter.Chain, *filter.Chain, error) {
	chains := map[string]*filter.Chain{}

	if len(job.Filters) == 0 {
		for _, d := range job.Destinations {
			chains[d.DestTargetID] = filter.NewChain(nil)
		}
		return chains, filter.NewChain(nil), nil
	}

	// Every rule file is read exactly once. Reading them twice — once for
	// the diff chains and once for the source-prune chain — would let the
	// two disagree if a file changed or a read failed in between, and a
	// source pruned more aggressively than the diff filters is precisely
	// how a mirror deletes a subtree it should have left alone.
	type resolved struct {
		rule    *filter.Rule
		scope   store.FilterScope
		target  string
		dropped string
	}

	compiled := make([]resolved, len(job.Filters))
	var degraded string

	for i := range job.Filters {
		rule := &job.Filters[i]
		compiled[i] = resolved{scope: rule.Scope, target: rule.ScopeTargetID}

		built, err := r.compileRule(ctx, rule)
		if err != nil {
			if rule.OnError == store.FilterFailRun {
				return nil, nil, fmt.Errorf("filter rule %d (%s): %w", i+1, rule.Describe(), err)
			}
			reason := fmt.Sprintf("rule %d (%s): %v", i+1, rule.Describe(), err)
			compiled[i].dropped = reason
			if degraded == "" {
				degraded = reason
			}
			events.Add(engine.Event{Level: store.LevelWarn, Message: fmt.Sprintf(
				"ignoring filter rule %d (%s): %v — deletions are disabled for this run because the filter is now narrower than configured",
				i+1, rule.Describe(), err)})
			continue
		}

		if built.PatternCount() == 0 {
			events.Add(engine.Event{Level: store.LevelWarn, Message: fmt.Sprintf(
				"filter rule %d (%s) resolved to no patterns; it matches nothing",
				i+1, rule.Describe())})
		}
		compiled[i].rule = built
	}

	build := func(include func(resolved) bool) *filter.Chain {
		var rules []*filter.Rule
		for _, c := range compiled {
			if c.rule != nil && include(c) {
				rules = append(rules, c.rule)
			}
		}
		chain := filter.NewChain(rules)
		if degraded != "" {
			chain.MarkDegraded(degraded)
		}
		return chain
	}

	for _, d := range job.Destinations {
		// Per-target rules layer on top of job-level ones, for that
		// destination only (SPEC.md §6.5).
		chains[d.DestTargetID] = build(func(c resolved) bool {
			return c.scope != store.ScopeTarget || c.target == d.DestTargetID
		})
	}

	// Only job-scoped rules may prune the shared source walk: the source is
	// scanned once for every destination, so pruning on a per-target rule
	// would corrupt the listing the others see.
	pruneChain := build(func(c resolved) bool { return c.scope == store.ScopeJob })

	return chains, pruneChain, nil
}

// compileRule turns one stored rule into a compiled one, reading its file if
// it has one.
func (r *Runner) compileRule(ctx context.Context, rule *store.FilterRule) (*filter.Rule, error) {
	patterns, err := r.rulePatterns(ctx, rule)
	if err != nil {
		return nil, err
	}

	direction := filter.Exclude
	if rule.Direction == store.FilterInclude {
		direction = filter.Include
	}
	return filter.CompileRule(rule.ID, rule.Describe(), direction, patterns, rule.CaseSensitive)
}

// rulePatterns produces a rule's patterns, reading its file if it has one.
func (r *Runner) rulePatterns(ctx context.Context, rule *store.FilterRule) ([]string, error) {
	if rule.Source == store.SourceInline {
		return rule.Patterns, nil
	}

	body, err := r.readRuleFile(ctx, rule.FilePath)
	if err != nil {
		return nil, err
	}

	if rule.Source == store.SourceListFile {
		return filter.ParseList(body), nil
	}
	return filter.PatternsAtKey(body, rule.JSONKey)
}

// readRuleFile reads a rule file from the container filesystem, or from a
// target when the path is a target:// reference.
func (r *Runner) readRuleFile(ctx context.Context, filePath string) ([]byte, error) {
	targetID, relPath, isRef := store.ParseTargetRef(strings.TrimSpace(filePath))
	if !isRef {
		return engine.ReadFileBounded(ctx, 0, filePath)
	}

	target, err := r.db.GetTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("the rule file lives on target %s, which could not be loaded: %w", targetID, err)
	}

	// Mounted only long enough to read the file, then released.
	root, _, release, err := r.resolve(ctx, target, "")
	if err != nil {
		return nil, fmt.Errorf("the rule file lives on %s, which is unavailable: %w", target.Describe(), err)
	}
	defer release()

	cleaned := filepath.Clean("/" + relPath)
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return nil, errors.New("the rule file path must not contain \"..\" path segments")
		}
	}
	return engine.ReadFileBounded(ctx, 0, filepath.Join(root, cleaned))
}
