package filter

import (
	"reflect"
	"strings"
	"testing"
)

// A catalogue keyed by something the user cannot predict, which is the shape
// the wildcard exists for. Note the hashes sort "7ee..." before "cb2...", so
// the expected order below is not the order they appear in the document.
const catalogue = `{
  "tracked_flags": {},
  "tracked_repo_assets": {
    "cb2cf6dbd5ecbcd83ac9aab1e4a85c45": {"name": "1205_A1_EvanOpening_v001.mov", "size": 594},
    "7ee451f5837d8174bea08f2b1cb7c86b": {"name": "1519_A1_RDJWalkOn_v000.mov", "size": 2475}
  },
  "untracked_repo_assets": {
    "aa11bb22cc33dd44ee55ff6677889900": {"name": "2001_B2_Finale_v003.mov"},
    "bb22cc33dd44ee55ff66778899001122": {"noname": true}
  }
}`

func TestPatternsAtKeyWildcardAndMultipleKeys(t *testing.T) {
	tests := []struct {
		name     string
		document string
		key      string
		want     []string
		wantErr  string
	}{
		{
			name:     "wildcard collects a field from every child",
			document: catalogue,
			key:      "tracked_repo_assets.*.name",
			want:     []string{"1519_A1_RDJWalkOn_v000.mov", "1205_A1_EvanOpening_v001.mov"},
		},
		{
			name:     "two keys are unioned in the order written",
			document: catalogue,
			key:      "tracked_repo_assets.*.name\nuntracked_repo_assets.*.name",
			want: []string{
				"1519_A1_RDJWalkOn_v000.mov",
				"1205_A1_EvanOpening_v001.mov",
				"2001_B2_Finale_v003.mov",
			},
		},
		{
			name:     "blank lines in the key field are ignored",
			document: catalogue,
			key:      "\n  tracked_repo_assets.*.name  \n\n",
			want:     []string{"1519_A1_RDJWalkOn_v000.mov", "1205_A1_EvanOpening_v001.mov"},
		},
		{
			name:     "an empty section contributes nothing rather than failing",
			document: catalogue,
			key:      "tracked_flags.*.name\ntracked_repo_assets.*.name",
			want:     []string{"1519_A1_RDJWalkOn_v000.mov", "1205_A1_EvanOpening_v001.mov"},
		},
		{
			name:     "a section that does not exist is skipped when the key is a wildcard",
			document: catalogue,
			key:      "no_such_section.*.name\ntracked_repo_assets.*.name",
			want:     []string{"1519_A1_RDJWalkOn_v000.mov", "1205_A1_EvanOpening_v001.mov"},
		},
		{
			name:     "a child missing the field is skipped, the rest are kept",
			document: catalogue,
			key:      "untracked_repo_assets.*.name",
			want:     []string{"2001_B2_Finale_v003.mov"},
		},
		{
			name:     "duplicates across keys are dropped",
			document: `{"a": {"x": {"name": "same.mov"}}, "b": {"y": {"name": "same.mov"}}}`,
			key:      "a.*.name\nb.*.name",
			want:     []string{"same.mov"},
		},
		{
			name:     "a wildcard that matches nothing is allowed",
			document: catalogue,
			key:      "tracked_flags.*.name",
			want:     []string{},
		},
		{
			name:     "a wildcard walks an array",
			document: `{"sets": [{"name": "a.mov"}, {"name": "b.mov"}]}`,
			key:      "sets.*.name",
			want:     []string{"a.mov", "b.mov"},
		},
		{
			name:     "a wildcard collects list values, not just scalars",
			document: `{"groups": {"one": {"skip": ["*.tmp", "cache/"]}, "two": {"skip": ["*.bak"]}}}`,
			key:      "groups.*.skip",
			want:     []string{"*.tmp", "cache/", "*.bak"},
		},
		{
			name:     "a non-string under a wildcard is skipped",
			document: `{"a": {"x": {"name": "keep.mov"}, "y": {"name": 42}}}`,
			key:      "a.*.name",
			want:     []string{"keep.mov"},
		},

		// The other half of the contract: without a wildcard a key is an
		// assertion, so a typo must still be an error rather than an empty
		// result that silently syncs nothing.
		{
			name:     "a literal key that is missing is still an error",
			document: catalogue,
			key:      "no_such_section.name",
			wantErr:  "does not exist",
		},
		{
			name:     "a literal key onto an object is still an error",
			document: catalogue,
			key:      "tracked_repo_assets",
			wantErr:  "must be a list of patterns",
		},
		{
			name:     "one bad literal key fails even when another key is fine",
			document: catalogue,
			key:      "tracked_repo_assets.*.name\nno_such_section.name",
			wantErr:  "does not exist",
		},
		{
			name:     "an empty segment is still an error",
			document: catalogue,
			key:      "tracked_repo_assets..*",
			wantErr:  "empty path segment",
		},
		{
			name:     "a key field of only blank lines is refused",
			document: catalogue,
			key:      "   \n\n  ",
			wantErr:  "at least one dot-path key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PatternsAtKey([]byte(tt.document), tt.key)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("PatternsAtKey succeeded with %v, want an error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PatternsAtKey: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("patterns = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Map iteration is randomised in Go, so an unsorted fan-out would give a
// different pattern order on different runs — which would make the run log and
// the rule's pattern count differ between two identical runs.
func TestPatternsAtKeyWildcardOrderIsStable(t *testing.T) {
	const key = "tracked_repo_assets.*.name"
	first, err := PatternsAtKey([]byte(catalogue), key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := PatternsAtKey([]byte(catalogue), key)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("iteration %d gave %v, first gave %v", i, again, first)
		}
	}
}
