package filter

import (
	"strings"
	"testing"
)

func mustCompile(t *testing.T, pattern string, caseSensitive bool) *Pattern {
	t.Helper()
	p, err := Compile(pattern, caseSensitive)
	if err != nil {
		t.Fatalf("Compile(%q): %v", pattern, err)
	}
	return p
}

func TestPatternMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// * stays within a segment.
		{"star matches within a segment", "*.tmp", "scratch.tmp", false, true},
		{"star matches at depth", "*.tmp", "a/b/scratch.tmp", false, true},
		{"star does not cross a separator", "a/*.tmp", "a/b/scratch.tmp", false, false},

		// ** crosses segments.
		{"doublestar crosses segments", "a/**/x.txt", "a/b/c/x.txt", false, true},
		{"doublestar matches zero segments", "a/**/x.txt", "a/x.txt", false, true},

		{"question mark matches one character", "file?.txt", "file1.txt", false, true},
		{"question mark needs a character", "file?.txt", "file.txt", false, false},

		// A leading slash anchors to the sync root.
		{"anchored matches at the root", "/build", "build", true, true},
		{"anchored does not match at depth", "/build", "src/build", true, false},
		{"unanchored matches at depth", "build", "src/build", true, true},

		// A trailing slash means directories only.
		{"directory rule matches a directory", "cache/", "cache", true, true},
		{"directory rule ignores a file", "cache/", "cache", false, false},
		{"directory rule matches at depth", "cache/", "a/b/cache", true, true},

		{"literal name at any depth", "Thumbs.db", "a/b/Thumbs.db", false, true},
		{"non-match", "*.tmp", "keep.txt", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mustCompile(t, tt.pattern, true)
			if got := p.Match(tt.path, tt.isDir); got != tt.want {
				t.Errorf("Match(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

// SMB targets are usually case-insensitive, so that is the default.
func TestPatternCaseSensitivity(t *testing.T) {
	tests := []struct {
		name          string
		caseSensitive bool
		pattern       string
		path          string
		want          bool
	}{
		{"insensitive matches other case", false, "*.TMP", "scratch.tmp", true},
		{"insensitive matches either way", false, "Cache/", "cache", true},
		{"sensitive rejects other case", true, "*.TMP", "scratch.tmp", false},
		{"sensitive matches exactly", true, "*.tmp", "scratch.tmp", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := mustCompile(t, tt.pattern, tt.caseSensitive)
			isDir := strings.HasSuffix(tt.pattern, "/")
			if got := p.Match(tt.path, isDir); got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestCompileRejectsNonsense(t *testing.T) {
	for _, pattern := range []string{"", "   ", "/"} {
		if _, err := Compile(pattern, false); err == nil {
			t.Errorf("Compile(%q) succeeded, want an error", pattern)
		}
	}
}

// A directory rule removes everything beneath it, not just the directory.
func TestDirectoryRulePrunesTheSubtree(t *testing.T) {
	rule, err := CompileRule("r1", "exclude cache", Exclude, []string{"cache/"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{rule})

	for _, path := range []string{"cache", "cache/x.bin", "cache/deep/y.bin", "a/cache/z.bin"} {
		isDir := path == "cache"
		if chain.Admits(path, isDir) {
			t.Errorf("%q was admitted; a directory rule must remove the whole subtree", path)
		}
	}
	if !chain.Admits("keep.txt", false) {
		t.Error("an unrelated file was excluded")
	}
	if !chain.PrunesDir("cache") {
		t.Error("PrunesDir did not report that the walk can skip cache/")
	}
}

// SPEC.md §6.5: includes define the universe, then excludes win over them.
func TestChainEvaluationOrder(t *testing.T) {
	includes, err := CompileRule("inc", "only documents", Include, []string{"*.doc", "*.txt"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	excludes, err := CompileRule("exc", "no drafts", Exclude, []string{"draft-*"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{includes, excludes})

	tests := []struct {
		path string
		want bool
	}{
		{"report.txt", true},
		{"report.doc", true},
		{"image.png", false},        // no include matched
		{"draft-report.txt", false}, // included, then excluded
	}
	for _, tt := range tests {
		if got := chain.Admits(tt.path, false); got != tt.want {
			t.Errorf("Admits(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// A chain of only excludes implicitly includes everything else.
func TestPureExcludeChainAdmitsTheRest(t *testing.T) {
	rule, err := CompileRule("r", "no temporaries", Exclude, []string{"*.tmp"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{rule})

	if !chain.Admits("anything.txt", false) {
		t.Error("a pure-exclude chain did not admit an unmatched path")
	}
	if chain.Admits("scratch.tmp", false) {
		t.Error("an excluded path was admitted")
	}
}

func TestEmptyChainAdmitsEverything(t *testing.T) {
	var nilChain *Chain
	if !nilChain.Admits("anything", false) {
		t.Error("a nil chain must admit everything")
	}
	if !NewChain(nil).Admits("anything", false) {
		t.Error("an empty chain must admit everything")
	}
}

// The run log records counts, not full path dumps (SPEC.md §6.5).
func TestChainCounts(t *testing.T) {
	rule, err := CompileRule("r1", "no temporaries", Exclude, []string{"*.tmp"}, false)
	if err != nil {
		t.Fatalf("CompileRule: %v", err)
	}
	chain := NewChain([]*Rule{rule})

	for _, path := range []string{"a.tmp", "b.tmp", "c.txt"} {
		chain.Admits(path, false)
	}

	counts := chain.Counts()
	if len(counts) != 1 {
		t.Fatalf("counts = %d, want 1", len(counts))
	}
	if counts[0].Excluded != 2 {
		t.Errorf("excluded = %d, want 2", counts[0].Excluded)
	}
	if counts[0].RuleID != "r1" {
		t.Errorf("rule id = %q", counts[0].RuleID)
	}
}

// List files allow comments and blank lines.
func TestParseList(t *testing.T) {
	got := ParseList([]byte("# a comment\n\n*.tmp\r\n  cache/  \n\n# another\nThumbs.db\n"))
	want := []string{"*.tmp", "cache/", "Thumbs.db"}

	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPatternsAtKey(t *testing.T) {
	tests := []struct {
		name     string
		document string
		key      string
		want     []string
		wantErr  string
	}{
		{
			name:     "nested object path",
			document: `{"backup": {"exclude": ["*.tmp", "cache/"]}}`,
			key:      "backup.exclude",
			want:     []string{"*.tmp", "cache/"},
		},
		{
			name:     "array index",
			document: `[{"name":"media","skip":["Thumbs.db"]}]`,
			key:      "0.skip",
			want:     []string{"Thumbs.db"},
		},
		{
			name:     "deep mixed path",
			document: `{"jobs": [{"filters": {"exclude": ["a", "b"]}}]}`,
			key:      "jobs.0.filters.exclude",
			want:     []string{"a", "b"},
		},
		{
			name:     "a single string is accepted",
			document: `{"exclude": "*.tmp"}`,
			key:      "exclude",
			want:     []string{"*.tmp"},
		},
		{
			name:     "empty list is valid",
			document: `{"exclude": []}`,
			key:      "exclude",
			want:     []string{},
		},
		{
			name:     "missing key",
			document: `{"backup": {}}`,
			key:      "backup.exclude",
			wantErr:  "does not exist",
		},
		{
			name:     "index out of range",
			document: `[{"skip":["x"]}]`,
			key:      "3.skip",
			wantErr:  "out of range",
		},
		{
			name:     "index into an object",
			document: `{"a": {"b": ["x"]}}`,
			key:      "a.0",
			wantErr:  "does not exist",
		},
		{
			name:     "name into an array",
			document: `{"a": ["x"]}`,
			key:      "a.name",
			wantErr:  "must be a number",
		},
		{
			name:     "value is a number",
			document: `{"exclude": 42}`,
			key:      "exclude",
			wantErr:  "must be a list of patterns",
		},
		{
			name:     "value is an object",
			document: `{"exclude": {"a": 1}}`,
			key:      "exclude",
			wantErr:  "must be a list of patterns",
		},
		{
			name:     "list contains a non-string",
			document: `{"exclude": ["ok", 7]}`,
			key:      "exclude",
			wantErr:  "must be a string",
		},
		{
			name:     "descending through a scalar",
			document: `{"a": 5}`,
			key:      "a.b",
			wantErr:  "not an object or an array",
		},
		{
			name:     "malformed json",
			document: `{not json`,
			key:      "a",
			wantErr:  "not valid JSON",
		},
		{
			name:     "empty path segment",
			document: `{"a": ["x"]}`,
			key:      "a..b",
			wantErr:  "empty path segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PatternsAtKey([]byte(tt.document), tt.key)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("PatternsAtKey succeeded, want an error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				// Errors reach the user, so they must name the key.
				if !strings.Contains(err.Error(), tt.key) && !strings.Contains(err.Error(), "JSON") {
					t.Errorf("error %q does not name the key %q", err, tt.key)
				}
				return
			}

			if err != nil {
				t.Fatalf("PatternsAtKey: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("patterns = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("pattern %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
