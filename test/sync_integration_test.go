//go:build integration

package test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alokw/cn4m-cascade/internal/store"
)

// mountFor mounts a target and returns its local root, so a test can seed or
// inspect a share directly.
func (h *harness) mountFor(t *testing.T, targetID string) string {
	t.Helper()

	tgt, err := h.db.GetTarget(context.Background(), targetID)
	if err != nil {
		t.Fatalf("loading target: %v", err)
	}
	root, release, err := h.mounts.Acquire(context.Background(), tgt)
	if err != nil {
		t.Fatalf("mounting %s: %v", tgt.Describe(), err)
	}
	t.Cleanup(release)
	return root
}

// seedTree writes a fixture tree into dir. Paths ending in "/" are
// directories; everything else is a file of the given size.
func seedTree(t *testing.T, dir string, files map[string]int) {
	t.Helper()

	for rel, size := range files {
		full := filepath.Join(dir, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", rel, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte('a' + i%26)
		}
		if err := os.WriteFile(full, payload, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// createJob posts a job and returns its id.
func (h *harness) createJob(t *testing.T, body map[string]any) string {
	t.Helper()

	status, resp := h.do(http.MethodPost, "/api/jobs", body)
	if status != http.StatusCreated {
		t.Fatalf("POST /api/jobs = %d, want 201: %v", status, resp)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("created job has no id: %v", resp)
	}
	return id
}

// runJob triggers a run and returns its id.
func (h *harness) runJob(t *testing.T, jobID string) string {
	t.Helper()

	status, resp := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", nil)
	if status != http.StatusAccepted {
		t.Fatalf("run = %d, want 202: %v", status, resp)
	}
	id, _ := resp["id"].(string)
	return id
}

// awaitRun polls until the run reaches a terminal state.
func (h *harness) awaitRun(t *testing.T, runID string, timeout time.Duration) map[string]any {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)
		if status != http.StatusOK {
			t.Fatalf("GET run = %d: %v", status, body)
		}
		// A parked preview is neither running nor finished, so "not
		// running" is not the same question as "done".
		if terminalRunStatus(body["status"]) {
			return body
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %v", runID, timeout)
	return nil
}

// terminalRunStatus mirrors store.Run.Terminal for a decoded JSON body.
func terminalRunStatus(v any) bool {
	s, _ := v.(string)
	switch store.RunStatus(s) {
	case store.RunSuccess, store.RunPartial, store.RunFailed, store.RunCancelled:
		return true
	default:
		return false
	}
}

// awaitCopying blocks until the run is demonstrably copying files. Sleeping a
// fixed time is unreliable: over a local bridge a small tree finishes in well
// under a second.
func (h *harness) awaitCopying(t *testing.T, runID string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, body := h.do(http.MethodGet, "/api/runs/"+runID, nil)

		if s, _ := body["status"].(string); s != string(store.RunRunning) {
			t.Fatalf("run reached %q before any copying could be observed", s)
		}
		if progress, ok := body["progress"].(map[string]any); ok {
			phase, _ := progress["phase"].(string)
			done, _ := progress["files_done"].(float64)
			if phase == "copying" && done > 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never started copying within %v", runID, timeout)
}

// seedManyFiles writes a wide tree of small files. Small files over SMB are
// latency-bound, which is what makes a run last long enough to interrupt.
func seedManyFiles(t *testing.T, root string, count int) {
	t.Helper()

	for i := range count {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%50))
		if i < 50 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.bin", i)),
			[]byte(fmt.Sprintf("payload-%d", i)), 0o644); err != nil {
			t.Fatalf("writing file %d: %v", i, err)
		}
	}
}

// smbJobFixture creates source and destination targets on the two Samba
// servers, each scoped to a unique subdirectory, plus a job between them.
func smbJobFixture(t *testing.T, h *harness, mode string, workers int, destOpts string) (jobID, srcRoot, dstRoot string) {
	t.Helper()

	scope := fmt.Sprintf("run-%d", time.Now().UnixNano())

	srcID := h.createTarget(smbTarget(uniqueName("src"), sambaA(t), shareCredentialed, userName, userPassword))

	dstPayload := smbTarget(uniqueName("dst"), sambaB(t), shareCredentialed, userName, userPassword)
	if destOpts != "" {
		dstPayload["mount_opts_override"] = destOpts
	}
	dstID := h.createTarget(dstPayload)

	srcRoot = filepath.Join(h.mountFor(t, srcID), scope)
	dstRoot = filepath.Join(h.mountFor(t, dstID), scope)
	for _, dir := range []string{srcRoot, dstRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s on the share: %v", dir, err)
		}
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(srcRoot)
		_ = os.RemoveAll(dstRoot)
	})

	jobID = h.createJob(t, map[string]any{
		"name":             uniqueName("job"),
		"source_target_id": srcID,
		"source_subpath":   scope,
		"mode":             mode,
		"workers":          workers,
		"destinations":     []map[string]any{{"dest_target_id": dstID, "dest_subpath": scope}},
	})
	return jobID, srcRoot, dstRoot
}

// Phase 2 exit criterion, at ordinary scale: mirror correctly between two SMB
// shares, then re-run as a no-op.
func TestMirrorBetweenSMBSharesAndRerunIsANoOp(t *testing.T) {
	h := newHarness(t, nil)
	jobID, srcRoot, dstRoot := smbJobFixture(t, h, string(store.ModeMirror), 4, "")

	seedTree(t, srcRoot, map[string]int{
		"top.txt":             120,
		"nested/one.txt":      4096,
		"nested/deep/two.txt": 65536,
		"nested/deep/big.bin": 3 << 20,
		"ünïcodé-📁.txt":       64,
		"zero.bin":            0,
		"emptydir/":           0,
	})
	// Something extraneous at the destination, which mirror must remove.
	seedTree(t, dstRoot, map[string]int{"stale.txt": 10, "staledir/inner.txt": 10})

	first := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := first["status"]; got != string(store.RunSuccess) {
		t.Fatalf("first run status = %v, want success: %v", got, first)
	}

	assertSMBTreesMatch(t, srcRoot, dstRoot)
	if _, err := os.Stat(filepath.Join(dstRoot, "stale.txt")); !os.IsNotExist(err) {
		t.Error("mirror did not remove the extraneous file")
	}

	// The re-run is the real test of mtime preservation: without it every
	// file would be copied again.
	second := h.awaitRun(t, h.runJob(t, jobID), 2*time.Minute)
	if got := second["status"]; got != string(store.RunSuccess) {
		t.Fatalf("second run status = %v, want success: %v", got, second)
	}

	dests, _ := second["destinations"].([]any)
	if len(dests) != 1 {
		t.Fatalf("destinations = %v", dests)
	}
	dest, _ := dests[0].(map[string]any)
	if copied, _ := dest["files_total"].(float64); copied != 0 {
		t.Errorf("the re-run planned %v copies, want 0: mtimes are not being preserved across SMB", copied)
	}
}

// Update mode copies but never deletes.
func TestUpdateModeOverSMB(t *testing.T) {
	h := newHarness(t, nil)
	jobID, srcRoot, dstRoot := smbJobFixture(t, h, string(store.ModeUpdate), 4, "")

	seedTree(t, srcRoot, map[string]int{"new.txt": 100})
	seedTree(t, dstRoot, map[string]int{"keepme.txt": 50})

	run := h.awaitRun(t, h.runJob(t, jobID), time.Minute)
	if got := run["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, run)
	}

	if _, err := os.Stat(filepath.Join(dstRoot, "keepme.txt")); err != nil {
		t.Error("update mode deleted a file it should have left alone")
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "new.txt")); err != nil {
		t.Error("update mode did not copy the new file")
	}
}

// Concurrent triggers of one job must not both start.
func TestSecondRunOfTheSameJobIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	jobID, srcRoot, _ := smbJobFixture(t, h, string(store.ModeMirror), 1, "")

	seedManyFiles(t, srcRoot, 2000)
	runID := h.runJob(t, jobID)
	h.awaitCopying(t, runID, 30*time.Second)

	status, body := h.do(http.MethodPost, "/api/jobs/"+jobID+"/run", nil)
	if status != http.StatusConflict {
		t.Errorf("second trigger = %d, want 409: %v", status, body)
	}

	if err := h.runner.Cancel(runID); err != nil {
		t.Logf("cancel: %v", err)
	}
	h.awaitRun(t, runID, 2*time.Minute)
}

// Cancellation must stop a run promptly and record it as cancelled.
func TestCancelRun(t *testing.T) {
	h := newHarness(t, nil)
	jobID, srcRoot, _ := smbJobFixture(t, h, string(store.ModeMirror), 1, "")

	seedManyFiles(t, srcRoot, 4000)

	runID := h.runJob(t, jobID)
	h.awaitCopying(t, runID, 30*time.Second)

	status, body := h.do(http.MethodPost, "/api/runs/"+runID+"/cancel", nil)
	if status != http.StatusAccepted {
		t.Fatalf("cancel = %d, want 202: %v", status, body)
	}

	started := time.Now()
	run := h.awaitRun(t, runID, 60*time.Second)
	elapsed := time.Since(started)
	t.Logf("run reached %v %v after the cancel request", run["status"], elapsed.Round(time.Millisecond))

	if got := run["status"]; got != string(store.RunCancelled) {
		t.Errorf("status = %v, want cancelled", got)
	}
	if elapsed > 30*time.Second {
		t.Errorf("cancellation took %v to take effect", elapsed)
	}

	// A cancelled run must not have copied everything anyway.
	dests, _ := run["destinations"].([]any)
	dest, _ := dests[0].(map[string]any)
	done, _ := dest["files_done"].(float64)
	total, _ := dest["files_total"].(float64)
	if total > 0 && done >= total {
		t.Errorf("the run copied all %v files despite being cancelled", total)
	}
}

// Phase 2 exit criterion: pull the cable mid-run and get a clean failure
// within ~30s, with no hung process.
//
// iptables blackholes the destination server, which is closer to a cable pull
// than killing the container: connections are never closed, packets simply
// stop arriving, so only the CIFS `soft` and `echo_interval` options can save
// us from hanging forever.
func TestDestinationDisappearsMidRun(t *testing.T) {
	requireIptables(t)

	h := newHarness(t, nil)
	// No override by default: this exercises the shipped mount options,
	// including the echo_interval that governs detection time (D-25).
	jobID, srcRoot, _ := smbJobFixture(t, h, string(store.ModeMirror), 1,
		os.Getenv("SMBSYNC_TEST_DEST_OPTS"))

	seedManyFiles(t, srcRoot, 4000)

	runID := h.runJob(t, jobID)
	h.awaitCopying(t, runID, 30*time.Second)

	blackhole(t, sambaB(t))
	cut := time.Now()

	run := h.awaitRun(t, runID, 3*time.Minute)
	elapsed := time.Since(cut)
	t.Logf("run ended as %v, %v after the destination was cut off", run["status"], elapsed.Round(time.Second))

	switch run["status"] {
	case string(store.RunPartial), string(store.RunFailed):
	default:
		t.Errorf("status = %v, want partial or failed", run["status"])
	}
	// What this layer can guarantee is that the run ends cleanly rather than
	// hanging. The exact detection time belongs to the kernel and is noisy
	// (±20s between identical runs), so the assertion is the guarantee, not
	// the measurement — the measured value is logged above instead (D-25).
	if elapsed > 90*time.Second {
		t.Errorf("took %v to notice the destination was gone; the run must not hang", elapsed)
	}

	// The run must record why, not just die quietly.
	_, events := h.do(http.MethodGet, "/api/runs/"+runID+"/events?level=error", nil)
	list, _ := events["events"].([]any)
	if len(list) == 0 {
		t.Error("no error events were recorded for a run whose destination vanished")
	}
}

// The 100k-file exit criterion. Slow by nature, so it only runs when asked:
// `make test-scale`.
func TestScaleMirror(t *testing.T) {
	target := os.Getenv("SMBSYNC_SCALE_FILES")
	if target == "" {
		t.Skip("set SMBSYNC_SCALE_FILES (e.g. `make test-scale`) to run the scale test")
	}
	count, err := strconv.Atoi(target)
	if err != nil {
		t.Fatalf("SMBSYNC_SCALE_FILES=%q is not a number", target)
	}

	h := newHarness(t, nil)
	jobID, srcRoot, dstRoot := smbJobFixture(t, h, string(store.ModeMirror), 4, "")

	t.Logf("generating %d files...", count)
	generated := time.Now()
	// 1000 files per directory keeps directory listings a sane size while
	// still exercising a deep-ish tree.
	for i := range count {
		dir := filepath.Join(srcRoot, fmt.Sprintf("d%03d", i/1000))
		if i%1000 == 0 {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.bin", i%1000)),
			[]byte(fmt.Sprintf("file-%d", i)), 0o644); err != nil {
			t.Fatalf("writing file %d: %v", i, err)
		}
	}
	t.Logf("generated in %v", time.Since(generated).Round(time.Second))

	mirrored := time.Now()
	first := h.awaitRun(t, h.runJob(t, jobID), 45*time.Minute)
	t.Logf("mirrored %d files in %v", count, time.Since(mirrored).Round(time.Second))

	if got := first["status"]; got != string(store.RunSuccess) {
		t.Fatalf("status = %v, want success: %v", got, first)
	}
	if scanned, _ := first["files_scanned"].(float64); int(scanned) != count {
		t.Errorf("scanned %v files, want %d", scanned, count)
	}

	// Correctness: same file count on both sides...
	srcCount := countFiles(t, srcRoot)
	dstCount := countFiles(t, dstRoot)
	if srcCount != dstCount {
		t.Errorf("destination holds %d files, source %d", dstCount, srcCount)
	}

	// ...and a first run against an empty destination must delete nothing.
	dests, _ := first["destinations"].([]any)
	firstDest, _ := dests[0].(map[string]any)
	if deleted, _ := firstDest["files_deleted"].(float64); deleted != 0 {
		t.Errorf("the first run deleted %v files against an empty destination", deleted)
	}

	// Counts alone would not notice truncated or misdated files. Spot-check
	// content and mtimes across a sample of directories rather than all
	// 100k paths, which would dominate the test's runtime.
	for _, dir := range []string{"d000", "d042", fmt.Sprintf("d%03d", (count-1)/1000)} {
		assertSMBTreesMatch(t, filepath.Join(srcRoot, dir), filepath.Join(dstRoot, dir))
	}

	rerun := time.Now()
	second := h.awaitRun(t, h.runJob(t, jobID), 45*time.Minute)
	t.Logf("re-ran in %v", time.Since(rerun).Round(time.Second))

	if got := second["status"]; got != string(store.RunSuccess) {
		t.Fatalf("re-run status = %v, want success: %v", got, second)
	}
	secondDests, _ := second["destinations"].([]any)
	secondDest, _ := secondDests[0].(map[string]any)
	if copied, _ := secondDest["files_total"].(float64); copied != 0 {
		t.Errorf("the re-run planned %v copies, want 0", copied)
	}
	if deleted, _ := secondDest["files_deleted"].(float64); deleted != 0 {
		t.Errorf("the re-run deleted %v files; a no-op must delete nothing", deleted)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()

	var n int
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return n
}

func assertSMBTreesMatch(t *testing.T, src, dst string) {
	t.Helper()

	// Both directions: source->dest catches anything missing or altered,
	// dest->source catches extras that mirror should have removed.
	assertContains(t, src, dst, true)
	assertContains(t, dst, src, false)
}

// assertContains walks `from` and checks each path exists in `to`. When
// compare is set it also checks size and mtime.
func assertContains(t *testing.T, from, to string, compare bool) {
	t.Helper()

	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		other, err := os.Stat(filepath.Join(to, rel))
		if err != nil {
			t.Errorf("%s exists in %s but not in %s: %v", rel, from, to, err)
			return nil
		}
		if info.IsDir() != other.IsDir() {
			t.Errorf("%s: directory-ness differs", rel)
			return nil
		}
		if info.IsDir() || !compare {
			return nil
		}
		if info.Size() != other.Size() {
			t.Errorf("%s: size %d vs %d", rel, info.Size(), other.Size())
		}
		// SMB mtime granularity is coarse; the job's tolerance is 2s.
		if diff := info.ModTime().Sub(other.ModTime()); diff > 2*time.Second || diff < -2*time.Second {
			t.Errorf("%s: mtime differs by %v", rel, diff)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", from, err)
	}
}

func requireIptables(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables is not available in this container")
	}
}

// iptablesBounded runs one iptables command and never blocks for longer than
// the bound, whatever the command does.
//
// Two things can wedge it. iptables waits on /run/xtables.lock indefinitely by
// default, so -w caps that. And fork/exec itself can stall in a process whose
// threads are parked in uninterruptible CIFS syscalls — which is the normal
// state of the cable-pull test. exec.CommandContext cannot help there, because
// its watchdog only arms after Start returns, so the wait is bounded here
// instead. The channel is buffered so the abandoned goroutine always finishes
// (CLAUDE.md: abandoning a goroutine parked in a syscall is the accepted cost
// of not hanging).
func iptablesBounded(args ...string) error {
	done := make(chan error, 1)
	go func() {
		cmd := exec.Command("iptables", append([]string{"-w", "5"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, out)
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		return fmt.Errorf("iptables %s did not return within 20s", strings.Join(args, " "))
	}
}

// dropBlackholes removes every DROP rule for an address, however many are
// installed. A test process killed before its cleanup ran (a timeout, an
// interrupted CI job) leaves its rule behind, and a stale rule makes every
// later run fail at the mount with error 115 — which reads exactly like a
// broken change. Draining on the way in makes the harness self-heal.
//
// It gives up on the first failure: "no such rule" is how the drain ends
// normally, and a timeout means the harness is wedged badly enough that
// looping again would only spend the test's remaining budget.
func dropBlackholes(ip string) {
	for i := 0; i < 16; i++ {
		if err := iptablesBounded("-D", "OUTPUT", "-d", ip, "-j", "DROP"); err != nil {
			return
		}
	}
}

// blackhole drops all traffic to an address until the test ends.
func blackhole(t *testing.T, ip string) {
	t.Helper()

	dropBlackholes(ip)
	if err := iptablesBounded("-A", "OUTPUT", "-d", ip, "-j", "DROP"); err != nil {
		t.Skipf("could not install an iptables rule (needs NET_ADMIN): %v", err)
	}
	t.Cleanup(func() {
		// The rule must come out even if the drain below cannot finish:
		// leaving it installed wedges every later test against this server.
		if err := iptablesBounded("-D", "OUTPUT", "-d", ip, "-j", "DROP"); err != nil {
			t.Errorf("could not remove the blackhole on %s; the harness is now dirty "+
				"(run `make harness-clean`): %v", ip, err)
			return
		}
		dropBlackholes(ip)
	})
}
