//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// fanOutFixture creates a source and two destinations, each scoped to a
// unique subdirectory, and returns the job payload builder for them.
func fanOutFixture(t *testing.T, h *harness) (srcID, dstAID, dstBID, scope, srcRoot string) {
	t.Helper()

	scope = fmt.Sprintf("fan-%d", time.Now().UnixNano())

	srcID = h.createTarget(smbTarget(uniqueName("src"), sambaA(t), shareCredentialed, userName, userPassword))
	dstAID = h.createTarget(smbTarget(uniqueName("dst-a"), sambaA(t), shareGuest, "", ""))
	dstBID = h.createTarget(smbTarget(uniqueName("dst-b"), sambaB(t), shareCredentialed, userName, userPassword))

	srcRoot = filepath.Join(h.mountFor(t, srcID), scope)
	for _, id := range []string{dstAID, dstBID} {
		root := filepath.Join(h.mountFor(t, id), scope)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("creating %s: %v", root, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatalf("creating the source scope: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(srcRoot) })

	return srcID, dstAID, dstBID, scope, srcRoot
}

// Phase 3 exit criterion: a job with two destinations where one is offline
// completes as `partial` under the skip policy.
func TestOneDestinationOfflineCompletesPartial(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"a.txt": 100, "nested/b.txt": 200})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("fanout"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"workers":            2,
		"unavailable_policy": string(store.PolicySkip),
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
	})

	// Cut off the second server before the run starts, so resolving it fails
	// at the availability gate rather than mid-copy.
	blackhole(t, sambaB(t))

	run := h.awaitRun(t, h.runJob(t, jobID), 3*time.Minute)

	if got := run["status"]; got != string(store.RunPartial) {
		t.Fatalf("status = %v, want partial: %v", got, run)
	}

	dests, _ := run["destinations"].([]any)
	if len(dests) != 2 {
		t.Fatalf("destinations = %d, want 2", len(dests))
	}

	byTarget := map[string]map[string]any{}
	for _, d := range dests {
		entry, _ := d.(map[string]any)
		id, _ := entry["dest_target_id"].(string)
		byTarget[id] = entry
	}

	if got := byTarget[dstAID]["status"]; got != string(store.DestSuccess) {
		t.Errorf("the reachable destination = %v, want success", got)
	}
	if got := byTarget[dstBID]["status"]; got != string(store.DestSkippedUnavailable) {
		t.Errorf("the offline destination = %v, want skipped_unavailable", got)
	}

	// The reachable destination must have received the files regardless.
	dstARoot := filepath.Join(h.mountFor(t, dstAID), scope)
	for _, want := range []string{"a.txt", "nested/b.txt"} {
		if _, err := os.Stat(filepath.Join(dstARoot, want)); err != nil {
			t.Errorf("%s did not reach the healthy destination: %v", want, err)
		}
	}
}

// Under abort, an unavailable destination fails the whole run.
func TestOneDestinationOfflineUnderAbort(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)
	seedTree(t, srcRoot, map[string]int{"a.txt": 100})

	jobID := h.createJob(t, map[string]any{
		"name":               uniqueName("abort"),
		"source_target_id":   srcID,
		"source_subpath":     scope,
		"mode":               string(store.ModeMirror),
		"unavailable_policy": string(store.PolicyAbort),
		"destinations": []map[string]any{
			{"dest_target_id": dstBID, "dest_subpath": scope},
			{"dest_target_id": dstAID, "dest_subpath": scope},
		},
	})

	blackhole(t, sambaB(t))
	run := h.awaitRun(t, h.runJob(t, jobID), 3*time.Minute)

	if got := run["status"]; got != string(store.RunFailed) {
		t.Fatalf("status = %v, want failed under the abort policy: %v", got, run)
	}
}

// Both destinations healthy: the run succeeds and both receive the tree.
func TestFanOutToTwoDestinations(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"a.txt":        100,
		"nested/b.txt": 200,
		"ünïcodé.txt":  50,
	})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("both"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"workers":          4,
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 3*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	for _, id := range []string{dstAID, dstBID} {
		root := filepath.Join(h.mountFor(t, id), scope)
		assertSMBTreesMatch(t, srcRoot, root)
	}
}

// Phase 3 exit criterion: a JSON-file exclude rule with a dot-path key
// demonstrably prunes a subtree.
func TestJSONFilterRulePrunesASubtree(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"keep.txt":            100,
		"docs/report.txt":     200,
		"cache/blob.bin":      5000,
		"cache/deep/more.bin": 5000,
	})

	// The rule file lives on the source share, referenced as target://.
	rulesPath := filepath.Join(srcRoot, "filters.json")
	if err := os.WriteFile(rulesPath, []byte(`{"backup": {"exclude": ["cache/", "*.tmp"]}}`), 0o644); err != nil {
		t.Fatalf("writing the rule file: %v", err)
	}

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("jsonfilter"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceJSONFile),
			"file_path": "target://" + srcID + "/" + scope + "/filters.json",
			"json_key":  "backup.exclude",
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	for _, want := range []string{"keep.txt", "docs/report.txt"} {
		if _, err := os.Stat(filepath.Join(dstRoot, want)); err != nil {
			t.Errorf("%s was not copied: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "cache")); !os.IsNotExist(err) {
		t.Error("the excluded subtree was copied to the destination")
	}
}

// The other half of the guarantee: an excluded subtree already at the
// destination is left alone, not deleted for looking extraneous.
func TestFilteredSubtreeAtTheDestinationIsNotDeleted(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"keep.txt": 100, "cache/fresh.bin": 10})

	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	seedTree(t, dstRoot, map[string]int{"cache/precious.bin": 4096})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("nodelete"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceInline),
			"patterns":  []string{"cache/"},
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	if _, err := os.Stat(filepath.Join(dstRoot, "cache", "precious.bin")); err != nil {
		t.Fatalf("an excluded file was deleted from the destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "keep.txt")); err != nil {
		t.Errorf("the in-scope file was not copied: %v", err)
	}
}

// Phase 3 exit criterion: a malformed key fails the run with a clear error.
func TestMalformedJSONKeyFailsTheRunClearly(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"a.txt": 10})
	rulesPath := filepath.Join(srcRoot, "filters.json")
	if err := os.WriteFile(rulesPath, []byte(`{"backup": {"exclude": ["cache/"]}}`), 0o644); err != nil {
		t.Fatalf("writing the rule file: %v", err)
	}

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("badkey"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceJSONFile),
			"file_path": "target://" + srcID + "/" + scope + "/filters.json",
			"json_key":  "backup.nosuchkey",
			"on_error":  string(store.FilterFailRun),
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := run["status"]; got != string(store.RunFailed) {
		t.Fatalf("status = %v, want failed: %v", got, run)
	}

	summary, _ := run["error_summary"].(string)
	for _, want := range []string{"backup.nosuchkey", "does not exist"} {
		if !strings.Contains(summary, want) {
			t.Errorf("error summary %q does not mention %q", summary, want)
		}
	}
	if strings.Contains(summary, "0x") || strings.Contains(summary, "*errors.") {
		t.Errorf("error summary %q reads like Go internals", summary)
	}

	// Nothing may have been copied: an unreadable exclude must stop the run
	// before it syncs files the user meant to exclude.
	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	if _, err := os.Stat(filepath.Join(dstRoot, "a.txt")); !os.IsNotExist(err) {
		t.Error("files were copied despite the filter failing")
	}
}

// ignore_rule downgrades the same failure to a warning.
func TestMalformedKeyWithIgnoreRulePolicy(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"a.txt": 10})
	if err := os.WriteFile(filepath.Join(srcRoot, "filters.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing the rule file: %v", err)
	}

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("ignorekey"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceJSONFile),
			"file_path": "target://" + srcID + "/" + scope + "/filters.json",
			"json_key":  "nope",
			"on_error":  string(store.FilterIgnoreRule),
		}},
	})

	runID := h.runJob(t, jobID)
	run := h.awaitRun(t, runID, 2*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success with the rule ignored: %v", got, run)
	}

	_, events := h.do(http.MethodGet, "/api/runs/"+runID+"/events?level=warn", nil)
	list, _ := events["events"].([]any)
	var explained bool
	for _, e := range list {
		entry, _ := e.(map[string]any)
		if msg, _ := entry["message"].(string); strings.Contains(msg, "ignoring filter rule") {
			explained = true
		}
	}
	if !explained {
		t.Error("the ignored rule was not explained in the run log")
	}
}

// Per-target rules give two destinations of one job different subsets.
func TestPerTargetFilterScoping(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"shared.txt": 10, "only-for-a.log": 20})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("scoped"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
		"filters": []map[string]any{{
			"scope":           string(store.ScopeTarget),
			"scope_target_id": dstBID,
			"direction":       string(store.FilterExclude),
			"source":          string(store.SourceInline),
			"patterns":        []string{"*.log"},
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 3*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	rootA := filepath.Join(h.mountFor(t, dstAID), scope)
	rootB := filepath.Join(h.mountFor(t, dstBID), scope)

	if _, err := os.Stat(filepath.Join(rootA, "only-for-a.log")); err != nil {
		t.Errorf("the unscoped destination is missing the log file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "only-for-a.log")); !os.IsNotExist(err) {
		t.Error("the scoped destination received a file its own rule excludes")
	}
	for _, root := range []string{rootA, rootB} {
		if _, err := os.Stat(filepath.Join(root, "shared.txt")); err != nil {
			t.Errorf("%s is missing the shared file: %v", root, err)
		}
	}
}

// The filter-test endpoint previews the decision without running anything.
func TestFilterTestEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"keep.txt":       10,
		"skip.tmp":       20,
		"cache/blob.bin": 30,
	})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("preview"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceInline),
			"patterns":  []string{"*.tmp", "cache/"},
		}},
	})

	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/filter-test", map[string]any{"sample_limit": 50})
	if status != http.StatusOK {
		t.Fatalf("filter-test = %d, want 200: %v", status, body)
	}

	dests, _ := body["destinations"].([]any)
	if len(dests) != 1 {
		t.Fatalf("destinations = %d, want 1", len(dests))
	}
	dest, _ := dests[0].(map[string]any)

	included := sampleNames(t, dest, "included")
	excluded := sampleNames(t, dest, "excluded")

	if !included["keep.txt"] {
		t.Errorf("keep.txt was not previewed as included: %v", included)
	}
	for _, want := range []string{"skip.tmp", "cache", "cache/blob.bin"} {
		if !excluded[want] {
			t.Errorf("%s was not previewed as excluded: %v", want, excluded)
		}
	}

	// Nothing should have been synced by a preview.
	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	if entries, err := os.ReadDir(dstRoot); err == nil && len(entries) != 0 {
		t.Errorf("the preview wrote to the destination: %v", entries)
	}
}

func sampleNames(t *testing.T, dest map[string]any, field string) map[string]bool {
	t.Helper()

	raw, _ := dest[field].([]any)
	names := map[string]bool{}
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		if name, ok := entry["relpath"].(string); ok {
			names[name] = true
		}
	}
	return names
}

// A rule that cannot be loaded is dropped under ignore_rule — and a dropped
// rule widens scope, so everything it protected at the destination would look
// extraneous. Deletions must be disabled for that run.
func TestDroppedFilterRuleDisablesDeletions(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{"keep.txt": 10})

	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	seedTree(t, dstRoot, map[string]int{"cache/precious.bin": 4096, "stale.txt": 20})

	// The rule file does not exist, so the rule cannot be loaded.
	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("degraded"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceListFile),
			"file_path": "target://" + srcID + "/" + scope + "/missing-rules.txt",
			"on_error":  string(store.FilterIgnoreRule),
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)

	// The excluded subtree must survive, and so must the ordinary
	// extraneous file: with the rule gone we cannot tell them apart.
	if _, err := os.Stat(filepath.Join(dstRoot, "cache", "precious.bin")); err != nil {
		t.Fatalf("a dropped filter rule caused a deletion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "stale.txt")); err != nil {
		t.Fatalf("deletions were not disabled after a rule was dropped: %v", err)
	}

	// And the run must say why rather than looking like a clean success.
	if got := run["status"]; got != string(store.RunPartial) {
		t.Errorf("status = %v, want partial", got)
	}
	summary, _ := run["error_summary"].(string)
	if !strings.Contains(summary, "could not be loaded") {
		t.Errorf("error summary %q does not explain that a rule was dropped", summary)
	}
}

// An include rule must copy into directories that no rule matches, which
// needs the mkdir to be derived from the copies rather than from directory
// admission.
func TestIncludeRuleCopiesIntoNewDirectories(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"photos/holiday.jpg": 100,
		"photos/notes.txt":   50,
		"docs/manual.txt":    70,
	})

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("includes"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeMirror),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterInclude),
			"source":    string(store.SourceInline),
			"patterns":  []string{"*.jpg"},
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)
	if _, err := os.Stat(filepath.Join(dstRoot, "photos", "holiday.jpg")); err != nil {
		t.Fatalf("the included file was not copied into its new directory: %v", err)
	}
	for _, unwanted := range []string{"photos/notes.txt", "docs/manual.txt"} {
		if _, err := os.Stat(filepath.Join(dstRoot, unwanted)); !os.IsNotExist(err) {
			t.Errorf("%s was copied despite not matching the include rule", unwanted)
		}
	}
}

// Parallel fan-out: two destinations at once, sharing one job-scoped rule.
func TestParallelDestinations(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, dstBID, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"a.txt":        100,
		"b.txt":        200,
		"skip.tmp":     300,
		"nested/c.txt": 400,
	})

	jobID := h.createJob(t, map[string]any{
		"name":                  uniqueName("parallel"),
		"source_target_id":      srcID,
		"source_subpath":        scope,
		"mode":                  string(store.ModeMirror),
		"parallel_destinations": true,
		"workers":               4,
		"destinations": []map[string]any{
			{"dest_target_id": dstAID, "dest_subpath": scope},
			{"dest_target_id": dstBID, "dest_subpath": scope},
		},
		"filters": []map[string]any{{
			"direction": string(store.FilterExclude),
			"source":    string(store.SourceInline),
			"patterns":  []string{"*.tmp"},
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 3*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	for _, id := range []string{dstAID, dstBID} {
		root := filepath.Join(h.mountFor(t, id), scope)
		for _, want := range []string{"a.txt", "b.txt", "nested/c.txt"} {
			if _, err := os.Stat(filepath.Join(root, want)); err != nil {
				t.Errorf("%s missing from a parallel destination: %v", want, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "skip.tmp")); !os.IsNotExist(err) {
			t.Errorf("the excluded file reached a parallel destination")
		}
	}
}

// The catalogue case (SPEC.md §6.5): a JSON file keyed by hashes the user
// cannot predict, addressed with "*", and two sections combined in one rule.
// This is an *include* rule, which is the direction where a mistake is quiet —
// a chain that resolves to nothing copies nothing and still reports success.
func TestJSONFilterWildcardKeysIncludeACatalogue(t *testing.T) {
	h := newHarness(t, nil)
	srcID, dstAID, _, scope, srcRoot := fanOutFixture(t, h)

	seedTree(t, srcRoot, map[string]int{
		"1200/1205_A1_EvanOpening_v001.mov": 400,
		"1500/1519_A1_RDJWalkOn_v000.mov":   500,
		"2000/2001_B2_Finale_v003.mov":      600,
		"1200/scratch_not_catalogued.mov":   700,
		"notes.txt":                         50,
	})

	// Hashes deliberately out of sorted order, and one entry with no "name",
	// so the fan-out has to sort and to skip.
	catalogue := `{
  "tracked_flags": {},
  "tracked_repo_assets": {
    "cb2cf6dbd5ecbcd83ac9aab1e4a85c45": {"name": "1205_A1_EvanOpening_v001.mov"},
    "7ee451f5837d8174bea08f2b1cb7c86b": {"name": "1519_A1_RDJWalkOn_v000.mov"}
  },
  "untracked_repo_assets": {
    "aa11bb22cc33dd44ee55ff6677889900": {"name": "2001_B2_Finale_v003.mov"},
    "bb22cc33dd44ee55ff66778899001122": {"pending": true}
  }
}`
	cataloguePath := filepath.Join(srcRoot, "catalogue.json")
	if err := os.WriteFile(cataloguePath, []byte(catalogue), 0o644); err != nil {
		t.Fatalf("writing the catalogue: %v", err)
	}

	jobID := h.createJob(t, map[string]any{
		"name":             uniqueName("jsonwildcard"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             string(store.ModeUpdate),
		"destinations":     []map[string]any{{"dest_target_id": dstAID, "dest_subpath": scope}},
		"filters": []map[string]any{{
			"direction": string(store.FilterInclude),
			"source":    string(store.SourceJSONFile),
			"file_path": "target://" + srcID + "/" + scope + "/catalogue.json",
			"json_key":  "tracked_repo_assets.*.name\nuntracked_repo_assets.*.name",
		}},
	})

	run := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	dstRoot := filepath.Join(h.mountFor(t, dstAID), scope)

	// Every catalogued asset, from both sections, found at its own depth.
	for _, want := range []string{
		"1200/1205_A1_EvanOpening_v001.mov",
		"1500/1519_A1_RDJWalkOn_v000.mov",
		"2000/2001_B2_Finale_v003.mov",
	} {
		if _, err := os.Stat(filepath.Join(dstRoot, want)); err != nil {
			t.Errorf("catalogued asset %s was not copied: %v", want, err)
		}
	}

	// Nothing else, or the include rule admitted more than the catalogue.
	for _, unwanted := range []string{"1200/scratch_not_catalogued.mov", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dstRoot, unwanted)); !os.IsNotExist(err) {
			t.Errorf("%s is not in the catalogue but was copied", unwanted)
		}
	}
}
