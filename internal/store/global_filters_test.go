package store

import (
	"context"
	"strings"
	"testing"
)

func TestGlobalFilterRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    GlobalFilterRule
		wantErr string
	}{
		{"inline with patterns", GlobalFilterRule{Source: SourceInline, Patterns: []string{"*.tmp"}}, ""},
		{"inline with none", GlobalFilterRule{Source: SourceInline}, "at least one pattern"},
		{"listfile needs a path", GlobalFilterRule{Source: SourceListFile}, "needs file_path"},
		{"listfile with a path", GlobalFilterRule{Source: SourceListFile, FilePath: "/tmp/x.txt"}, ""},
		{"jsonfile needs a key", GlobalFilterRule{Source: SourceJSONFile, FilePath: "/tmp/x.json"}, "needs json_key"},
		{"jsonfile complete", GlobalFilterRule{Source: SourceJSONFile, FilePath: "/tmp/x.json", JSONKey: "a.b"}, ""},
		{"unknown source", GlobalFilterRule{Source: "elsewhere"}, "source must be"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := tc.rule
			rule.ApplyDefaults()
			err := rule.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// A global rule is job-wide and exclude-only by construction. Nothing in the
// type, the payload or the schema can express otherwise — an include would
// widen every job rather than narrow it, and a target scope would let a
// "global" rule apply to one destination.
func TestGlobalFilterRuleProjectsAsAJobWideExclude(t *testing.T) {
	g := GlobalFilterRule{Source: SourceInline, Patterns: []string{"*.tmp"}}
	r := g.AsFilterRule()

	if r.Scope != ScopeJob {
		t.Errorf("scope = %q, want %q", r.Scope, ScopeJob)
	}
	if r.Direction != FilterExclude {
		t.Errorf("direction = %q, want %q", r.Direction, FilterExclude)
	}
	if r.ScopeTargetID != "" {
		t.Errorf("scope_target_id = %q, want empty", r.ScopeTargetID)
	}
}

func TestGlobalFilterRulesRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	// A fresh database carries the seeded defaults.
	seeded, err := db.ListGlobalFilterRules(ctx)
	if err != nil {
		t.Fatalf("ListGlobalFilterRules: %v", err)
	}
	if len(seeded) == 0 {
		t.Fatal("a fresh database has no seeded global rules")
	}

	want := []GlobalFilterRule{
		{Source: SourceInline, Patterns: []string{"*.tmp", "cache/"}, OnError: FilterFailRun},
		{Source: SourceListFile, FilePath: "/tmp/excludes.txt", OnError: FilterIgnoreRule},
	}
	if err := db.ReplaceGlobalFilterRules(ctx, want); err != nil {
		t.Fatalf("ReplaceGlobalFilterRules: %v", err)
	}

	got, err := db.ListGlobalFilterRules(ctx)
	if err != nil {
		t.Fatalf("ListGlobalFilterRules: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2", len(got))
	}
	// Position comes from array order: reordering the list is how evaluation
	// order is changed, so it has to survive the round trip.
	if got[0].Position != 0 || got[1].Position != 1 {
		t.Fatalf("positions = %d, %d; want 0, 1", got[0].Position, got[1].Position)
	}
	if strings.Join(got[0].Patterns, ",") != "*.tmp,cache/" {
		t.Fatalf("patterns = %v", got[0].Patterns)
	}
	if got[1].FilePath != "/tmp/excludes.txt" {
		t.Fatalf("file_path = %q", got[1].FilePath)
	}
}

// Replacement is wholesale, so an empty list clears everything — including the
// seed. That is how someone opts out of the defaults.
func TestReplaceGlobalFilterRulesWithNoneClearsThem(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	if err := db.ReplaceGlobalFilterRules(ctx, nil); err != nil {
		t.Fatalf("ReplaceGlobalFilterRules(nil): %v", err)
	}
	got, err := db.ListGlobalFilterRules(ctx)
	if err != nil {
		t.Fatalf("ListGlobalFilterRules: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rules after clearing, want 0", len(got))
	}
}

// Validation runs over the whole set before anything is written, so one bad
// rule cannot leave the list half-replaced — a partially applied global set
// applies to every job at once.
func TestReplaceGlobalFilterRulesValidatesBeforeMutating(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	before, err := db.ListGlobalFilterRules(ctx)
	if err != nil {
		t.Fatalf("ListGlobalFilterRules: %v", err)
	}

	err = db.ReplaceGlobalFilterRules(ctx, []GlobalFilterRule{
		{Source: SourceInline, Patterns: []string{"*.ok"}},
		{Source: SourceListFile}, // no file_path
	})
	if err == nil {
		t.Fatal("a rule with no file_path should be rejected")
	}
	if !strings.Contains(err.Error(), "rule 2") {
		t.Errorf("error does not name the offending rule: %v", err)
	}

	after, err := db.ListGlobalFilterRules(ctx)
	if err != nil {
		t.Fatalf("ListGlobalFilterRules: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a rejected replace still mutated the set: %d rules before, %d after",
			len(before), len(after))
	}
}
