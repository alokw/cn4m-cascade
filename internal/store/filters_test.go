package store

import (
	"context"
	"strings"
	"testing"
)

func TestFilterRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    FilterRule
		wantErr string
	}{
		{
			name: "valid inline exclude",
			rule: FilterRule{Direction: FilterExclude, Source: SourceInline, Patterns: []string{"*.tmp"}},
		},
		{
			name: "valid list file",
			rule: FilterRule{Direction: FilterExclude, Source: SourceListFile, FilePath: "/data/excludes.txt"},
		},
		{
			name: "valid json file",
			rule: FilterRule{Direction: FilterExclude, Source: SourceJSONFile, FilePath: "/data/f.json", JSONKey: "backup.exclude"},
		},
		{
			name: "valid target reference",
			rule: FilterRule{Direction: FilterExclude, Source: SourceListFile, FilePath: "target://abc123/conf/excludes.txt"},
		},
		{
			name:    "inline with no patterns",
			rule:    FilterRule{Direction: FilterExclude, Source: SourceInline},
			wantErr: "at least one pattern",
		},
		{
			name:    "list file with no path",
			rule:    FilterRule{Direction: FilterExclude, Source: SourceListFile},
			wantErr: "needs file_path",
		},
		{
			name:    "json file with no key",
			rule:    FilterRule{Direction: FilterExclude, Source: SourceJSONFile, FilePath: "/data/f.json"},
			wantErr: "needs json_key",
		},
		{
			name:    "unknown source",
			rule:    FilterRule{Direction: FilterExclude, Source: "sqlite"},
			wantErr: "source must be",
		},
		{
			name:    "unknown direction",
			rule:    FilterRule{Direction: "maybe", Source: SourceInline, Patterns: []string{"x"}},
			wantErr: "direction must be",
		},
		{
			name:    "target scope with no target",
			rule:    FilterRule{Scope: ScopeTarget, Direction: FilterExclude, Source: SourceInline, Patterns: []string{"x"}},
			wantErr: "scope_target_id is required",
		},
		{
			name: "job scope with a target",
			rule: FilterRule{
				Scope: ScopeJob, ScopeTargetID: "abc", Direction: FilterExclude,
				Source: SourceInline, Patterns: []string{"x"},
			},
			wantErr: "only valid when scope",
		},
		{
			name:    "inline with a file path",
			rule:    FilterRule{Direction: FilterExclude, Source: SourceInline, Patterns: []string{"x"}, FilePath: "/f"},
			wantErr: "only valid for a listfile or jsonfile",
		},
		{
			name:    "malformed target reference",
			rule:    FilterRule{Direction: FilterExclude, Source: SourceListFile, FilePath: "target://onlyid"},
			wantErr: "not a valid target reference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := tt.rule
			rule.ApplyDefaults()
			err := rule.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// An exclude that cannot be read must fail the run by default: silently
// syncing files the user meant to exclude is the dangerous direction.
func TestFilterRuleDefaultOnError(t *testing.T) {
	exclude := FilterRule{Direction: FilterExclude, Source: SourceInline, Patterns: []string{"x"}}
	exclude.ApplyDefaults()
	if exclude.OnError != FilterFailRun {
		t.Errorf("exclude on_error = %q, want %q", exclude.OnError, FilterFailRun)
	}

	// Includes default the same way: dropping the only include rule leaves
	// an empty include set, which admits everything.
	include := FilterRule{Direction: FilterInclude, Source: SourceInline, Patterns: []string{"x"}}
	include.ApplyDefaults()
	if include.OnError != FilterFailRun {
		t.Errorf("include on_error = %q, want %q", include.OnError, FilterFailRun)
	}
}

func TestParseTargetRef(t *testing.T) {
	tests := []struct {
		in       string
		wantID   string
		wantPath string
		wantOK   bool
	}{
		{"target://abc123/conf/excludes.txt", "abc123", "conf/excludes.txt", true},
		{"target://abc123/a.txt", "abc123", "a.txt", true},
		{"/data/excludes.txt", "", "", false},
		{"target://abc123", "", "", false},
		{"target:///nope.txt", "", "", false},
		{"target://", "", "", false},
	}

	for _, tt := range tests {
		id, p, ok := ParseTargetRef(tt.in)
		if ok != tt.wantOK || id != tt.wantID || p != tt.wantPath {
			t.Errorf("ParseTargetRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, id, p, ok, tt.wantID, tt.wantPath, tt.wantOK)
		}
	}
}

func TestFilterRulesRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	job := newTestJob(src, dst)
	job.Filters = []FilterRule{
		{Direction: FilterExclude, Source: SourceInline, Patterns: []string{"*.tmp", "cache/"}},
		{
			Scope: ScopeTarget, ScopeTargetID: dst,
			Direction: FilterInclude, Source: SourceJSONFile,
			FilePath: "target://" + src + "/conf/filters.json", JSONKey: "backup.include",
			CaseSensitive: true,
		},
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	loaded, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(loaded.Filters) != 2 {
		t.Fatalf("filters = %d, want 2", len(loaded.Filters))
	}

	first := loaded.Filters[0]
	if first.Scope != ScopeJob || first.Direction != FilterExclude || first.Source != SourceInline {
		t.Errorf("first rule = %+v", first)
	}
	if len(first.Patterns) != 2 || first.Patterns[0] != "*.tmp" {
		t.Errorf("first rule patterns = %v", first.Patterns)
	}
	if first.OnError != FilterFailRun {
		t.Errorf("first rule on_error = %q", first.OnError)
	}

	second := loaded.Filters[1]
	if second.Scope != ScopeTarget || second.ScopeTargetID != dst {
		t.Errorf("second rule scope = %q/%q", second.Scope, second.ScopeTargetID)
	}
	if second.JSONKey != "backup.include" || !second.CaseSensitive {
		t.Errorf("second rule = %+v", second)
	}
	// Evaluation order must be preserved.
	if first.Position != 0 || second.Position != 1 {
		t.Errorf("positions = %d, %d", first.Position, second.Position)
	}
}

// Deleting a job takes its filter rules with it.
func TestFilterRulesCascade(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	job := newTestJob(src, dst)
	job.Filters = []FilterRule{{Direction: FilterExclude, Source: SourceInline, Patterns: []string{"*.tmp"}}}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := db.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	rules, err := db.ListFilterRules(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListFilterRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("filter rules survived their job: %+v", rules)
	}
}

// Multi-destination fan-out persists.
func TestJobWithSeveralDestinations(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dstA := testTargetPair(t, db)

	dstB := &Target{Name: "dst-b", Type: TargetSMB, Host: "10.0.0.3", Share: "second"}
	if err := db.CreateTarget(ctx, dstB); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	job := newTestJob(src, dstA)
	job.Destinations = append(job.Destinations, JobDestination{DestTargetID: dstB.ID})
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	loaded, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(loaded.Destinations) != 2 {
		t.Fatalf("destinations = %d, want 2", len(loaded.Destinations))
	}
	if loaded.Destinations[0].Position != 0 || loaded.Destinations[1].Position != 1 {
		t.Errorf("destination order not preserved: %+v", loaded.Destinations)
	}
	if loaded.UnavailablePolicy != PolicySkip {
		t.Errorf("unavailable_policy = %q, want %q", loaded.UnavailablePolicy, PolicySkip)
	}
}
