package filter

import (
	"strings"
	"sync"
	"testing"
)

// PrunesDir and Admits must agree about everything beneath a directory.
//
// They did not: an exclude written without a trailing slash pruned the walk
// of "cache/" but still admitted "cache/x.bin". One side of the diff then saw
// a subtree the other did not, and in mirror mode that difference deletes.
func TestPruneAndAdmitAgreeOnSubtrees(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
	}{
		{"with a trailing slash", "cache/"},
		{"without a trailing slash", "cache"},
		{"anchored", "/cache"},
		{"glob", "ca*he"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := CompileRule("r", "exclude cache", Exclude, []string{tt.pattern}, false)
			if err != nil {
				t.Fatalf("CompileRule: %v", err)
			}
			chain := NewChain([]*Rule{rule})

			pruned := chain.PrunesDir("cache")
			admittedChild := chain.Admits("cache/x.bin", false)
			admittedDeep := chain.Admits("cache/deep/y.bin", false)

			if pruned && (admittedChild || admittedDeep) {
				t.Fatalf("pattern %q prunes the directory but admits its contents "+
					"(child=%v deep=%v); the two sides of the diff would disagree",
					tt.pattern, admittedChild, admittedDeep)
			}
		})
	}
}

// A chain that had to drop a rule must say so, because the differ refuses to
// delete anything from a degraded chain.
func TestChainDegraded(t *testing.T) {
	chain := NewChain(nil)
	if degraded, _ := chain.Degraded(); degraded {
		t.Fatal("a fresh chain reported itself degraded")
	}

	chain.MarkDegraded("the rule file was unreachable")
	degraded, reason := chain.Degraded()
	if !degraded {
		t.Fatal("MarkDegraded did not take effect")
	}
	if !strings.Contains(reason, "unreachable") {
		t.Errorf("reason = %q, want it to explain the cause", reason)
	}

	var nilChain *Chain
	if degraded, _ := nilChain.Degraded(); degraded {
		t.Error("a nil chain reported itself degraded")
	}
}

// An empty pattern list is ordinary live configuration, not a malformed rule:
// a JSON array that happens to be empty, or a list file of only comments.
func TestEmptyPatternListIsNotAnError(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
	}{
		{"empty list", nil},
		{"only comments", []string{"# nothing here", "   ", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := CompileRule("r", "empty", Exclude, tt.patterns, false)
			if err != nil {
				t.Fatalf("CompileRule: %v", err)
			}
			if rule.PatternCount() != 0 {
				t.Fatalf("patterns = %d, want 0", rule.PatternCount())
			}

			// An exclude that matches nothing removes nothing.
			if !NewChain([]*Rule{rule}).Admits("anything.txt", false) {
				t.Error("an empty exclude rule excluded a path")
			}
		})
	}
}

// An empty *include* list means an empty universe, not "no rule". Treating it
// as no rule would admit everything, and in mirror mode make every
// out-of-scope destination file extraneous.
func TestEmptyIncludeListAdmitsNothing(t *testing.T) {
	rule, err := CompileRule("r", "empty include", Include, nil, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	if NewChain([]*Rule{rule}).Admits("anything.txt", false) {
		t.Fatal("an empty include rule admitted a path; the universe should be empty")
	}
}

// Two chains built from one rule keep separate tallies, so a job-scoped rule
// reports per-destination counts rather than a running total.
func TestChainsDoNotShareCounters(t *testing.T) {
	rule, err := CompileRule("r", "no temporaries", Exclude, []string{"*.tmp"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}

	first := NewChain([]*Rule{rule})
	second := NewChain([]*Rule{rule})

	first.Admits("a.tmp", false)
	first.Admits("b.tmp", false)
	second.Admits("c.tmp", false)

	if got := first.Counts()[0].Excluded; got != 2 {
		t.Errorf("first chain excluded = %d, want 2", got)
	}
	if got := second.Counts()[0].Excluded; got != 1 {
		t.Errorf("second chain excluded = %d, want 1: chains must not share counters", got)
	}
}

// Pruning skips a whole subtree without visiting its paths, so it has to be
// counted where it happens or the rule reports zero for its biggest effect.
func TestPruningIsCounted(t *testing.T) {
	rule, err := CompileRule("r", "exclude cache", Exclude, []string{"cache/"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{rule})

	if !chain.PrunesDir("cache") {
		t.Fatal("the directory was not pruned")
	}
	if got := chain.Counts()[0].Excluded; got == 0 {
		t.Error("a pruned directory was not counted; the rule would report doing nothing")
	}
}

// The filter preview reports real counts, not zeros.
func TestExplainCounts(t *testing.T) {
	rule, err := CompileRule("r", "no temporaries", Exclude, []string{"*.tmp"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{rule})

	if _, matched := chain.Explain("a.tmp", false); matched == nil {
		t.Error("Explain did not name the rule responsible")
	}
	chain.Explain("b.txt", false)

	counts := chain.Counts()[0]
	if counts.Excluded != 1 {
		t.Errorf("excluded = %d, want 1", counts.Excluded)
	}
}

// Counters are mutated during matching, and the parallel fan-out matches from
// several goroutines at once.
func TestConcurrentMatchingIsSafe(t *testing.T) {
	rule, err := CompileRule("r", "no temporaries", Exclude, []string{"*.tmp"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}

	// Two chains from one rule, as a job-scoped rule across two
	// destinations running in parallel.
	chains := []*Chain{NewChain([]*Rule{rule}), NewChain([]*Rule{rule})}

	var wg sync.WaitGroup
	for _, chain := range chains {
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 200 {
					chain.Admits("file.tmp", false)
					chain.PrunesDir("dir")
					if i%10 == 0 {
						chain.Counts()
					}
				}
			}()
		}
	}
	wg.Wait()

	for i, chain := range chains {
		if got := chain.Counts()[0].Excluded; got != 8*200 {
			t.Errorf("chain %d excluded = %d, want %d", i, got, 8*200)
		}
	}
}
