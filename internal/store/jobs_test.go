package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// testTargetPair creates a source and destination target, since jobs carry
// foreign keys to them.
func testTargetPair(t *testing.T, db *DB) (src, dst string) {
	t.Helper()
	ctx := context.Background()

	source := &Target{Name: "src", Type: TargetSMB, Host: "10.0.0.1", Share: "media"}
	dest := &Target{Name: "dst", Type: TargetSMB, Host: "10.0.0.2", Share: "backup"}
	for _, tgt := range []*Target{source, dest} {
		if err := db.CreateTarget(ctx, tgt); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}
	return source.ID, dest.ID
}

func newTestJob(src, dst string) *Job {
	return &Job{
		Name:           "nightly",
		SourceTargetID: src,
		Mode:           ModeMirror,
		Destinations:   []JobDestination{{DestTargetID: dst}},
	}
}

func TestJobRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	job := newTestJob(src, dst)
	job.SourceSubpath = "photos"
	job.Destinations[0].DestSubpath = "archive/photos"
	job.Workers = 8
	job.IgnoreDSTHour = true
	job.LogEveryFile = true

	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.ID == "" {
		t.Fatal("CreateJob did not assign an ID")
	}

	loaded, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"name", loaded.Name, "nightly"},
		{"source", loaded.SourceTargetID, src},
		{"source subpath", loaded.SourceSubpath, "photos"},
		{"mode", loaded.Mode, ModeMirror},
		{"workers", loaded.Workers, 8},
		{"ignore dst", loaded.IgnoreDSTHour, true},
		{"log every file", loaded.LogEveryFile, true},
		{"destinations", len(loaded.Destinations), 1},
	} {
		if f.got != f.want {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if loaded.Destinations[0].DestSubpath != "archive/photos" {
		t.Errorf("dest subpath = %q", loaded.Destinations[0].DestSubpath)
	}
}

// Defaults come from SPEC.md §6.1/§6.2/§6.3.
func TestJobDefaults(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	job := &Job{Name: "defaults", SourceTargetID: src, Destinations: []JobDestination{{DestTargetID: dst}}}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	loaded, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"mode", loaded.Mode, ModeMirror},
		{"compare", loaded.Compare, CompareFast},
		{"tolerance", loaded.CompareToleranceSec, 2},
		{"workers", loaded.Workers, DefaultWorkers},
		{"on_error", loaded.OnError, ErrorPolicySkip},
		{"delete_policy", loaded.DeletePolicy, DeletePolicySkipDeletes},
	} {
		if f.got != f.want {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
}

func TestJobValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Job)
		wantErr string
	}{
		{"valid", func(*Job) {}, ""},
		{"update mode is valid", func(j *Job) { j.Mode = ModeUpdate }, ""},
		{"no name", func(j *Job) { j.Name = "" }, "name is required"},
		{"no source", func(j *Job) { j.SourceTargetID = "" }, "source target is required"},
		{"unknown mode", func(j *Job) { j.Mode = "sideways" }, "mode must be"},
		{"twoway not yet", func(j *Job) { j.Mode = "twoway" }, "not implemented yet"},
		{"content compare not yet", func(j *Job) { j.Compare = CompareContent }, "not implemented yet"},
		{"unknown on_error", func(j *Job) { j.OnError = "explode" }, "on_error must be"},
		{"unknown delete_policy", func(j *Job) { j.DeletePolicy = "yolo" }, "delete_policy must be"},
		{"too few workers", func(j *Job) { j.Workers = 0 }, "workers must be between"},
		{"too many workers", func(j *Job) { j.Workers = 99 }, "workers must be between"},
		{"negative tolerance", func(j *Job) { j.CompareToleranceSec = -1 }, "must not be negative"},
		{"no destination", func(j *Job) { j.Destinations = nil }, "at least one destination"},
		{
			"fan-out is not supported yet",
			func(j *Job) { j.Destinations = append(j.Destinations, JobDestination{DestTargetID: "other"}) },
			"only one destination",
		},
		{
			"destination same as source",
			func(j *Job) { j.Destinations[0].DestTargetID = j.SourceTargetID },
			"must not be the same location",
		},
		{"absolute source subpath", func(j *Job) { j.SourceSubpath = "/etc" }, "relative to the target root"},
		{"escaping dest subpath", func(j *Job) { j.Destinations[0].DestSubpath = "a/../.." }, `".." path segments`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := newTestJob("src-id", "dst-id")
			job.ApplyDefaults()
			tt.mutate(job)

			err := job.Validate()
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

func TestJobNameIsUnique(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	if err := db.CreateJob(ctx, newTestJob(src, dst)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := db.CreateJob(ctx, newTestJob(src, dst)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate job name = %v, want ErrNameTaken", err)
	}
}

func TestDeleteJobCascadesDestinations(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	src, dst := testTargetPair(t, db)

	job := newTestJob(src, dst)
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := db.DeleteJob(ctx, job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	dests, err := db.jobDestinations(ctx, job.ID)
	if err != nil {
		t.Fatalf("jobDestinations: %v", err)
	}
	if len(dests) != 0 {
		t.Fatalf("destinations survived the job: %v", dests)
	}
}

// A job may not reference a target that does not exist.
func TestJobRequiresARealSourceTarget(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, dst := testTargetPair(t, db)

	err := db.CreateJob(ctx, newTestJob("no-such-target", dst))
	if err == nil {
		t.Fatal("CreateJob accepted a source target that does not exist")
	}
}
