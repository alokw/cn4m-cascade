package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alokw/cn4m-cascade/internal/health"
	"github.com/alokw/cn4m-cascade/internal/store"
)

func localTarget(t *testing.T, root, subpath string) *LocalStorage {
	t.Helper()
	return &LocalStorage{
		target: &store.Target{
			ID: "t1", Name: "my-folder", Type: store.TargetLocal,
			LocalPath: root, Subpath: subpath,
		},
		healthc: health.NewCache(),
	}
}

// A missing *subpath* must not be reported as a missing target. The two read
// identically if the error only ever names the target's own root — which is
// how a job with a wrong subpath produces "the target does not exist" about a
// target that tests green seconds earlier.
func TestLocalMissingSubpathNamesThePathThatIsMissing(t *testing.T) {
	root := t.TempDir()

	// The target itself resolves.
	if _, err := localTarget(t, root, "").Resolve(context.Background()); err != nil {
		t.Fatalf("the target root should resolve: %v", err)
	}

	// A subpath under it does not.
	_, err := localTarget(t, root, "nope").Resolve(context.Background())
	if err == nil {
		t.Fatal("resolving a missing subpath should fail")
	}

	want := filepath.Join(root, "nope")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error does not name the path that is actually missing:\n  got:  %s\n  want it to contain: %s",
			err.Error(), want)
	}
	// The bind-mount hint belongs on a missing *root*, not on a missing
	// subpath — the mount is plainly working if the root resolved.
	if strings.Contains(err.Error(), "bind-mounted") {
		t.Fatalf("a missing subpath blamed the bind mount: %s", err.Error())
	}
}

// A subpath under a root that is ALSO missing must blame the root, not the
// subpath. Both are absent, so reporting the subpath as merely "does not
// exist" is technically true and practically wrong: the job editor offers to
// create a missing folder on the strength of that error (api.path_not_found),
// and creating one inside a broken bind mount is exactly the papering-over
// that CreateSubpath exists to prevent.
func TestLocalMissingSubpathUnderMissingRootBlamesTheRoot(t *testing.T) {
	absentRoot := filepath.Join(t.TempDir(), "absent")

	_, err := localTarget(t, absentRoot, "nope").Resolve(context.Background())
	if err == nil {
		t.Fatal("resolving a subpath under a missing root should fail")
	}
	if !strings.Contains(err.Error(), "bind-mounted") {
		t.Fatalf("a missing root was reported as a missing subpath: %s", err.Error())
	}
	if errors.Is(err, ErrPathNotExist) {
		t.Fatalf("a broken target matched ErrPathNotExist, so a caller would offer to create it: %s", err.Error())
	}
}

// A missing root keeps the hint, because that is the case it explains.
func TestLocalMissingRootMentionsTheBindMount(t *testing.T) {
	_, err := localTarget(t, filepath.Join(t.TempDir(), "absent"), "").Resolve(context.Background())
	if err == nil {
		t.Fatal("resolving a missing root should fail")
	}
	if !strings.Contains(err.Error(), "bind-mounted") {
		t.Fatalf("a missing root did not mention the bind mount: %s", err.Error())
	}
}

// A file where a directory belongs is its own failure, and must say so rather
// than claiming the path is absent.
func TestLocalRejectsAFileAsARoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	_, err := localTarget(t, file, "").Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is a file, not a directory") {
		t.Fatalf("resolving a file as a target root = %v", err)
	}
}
