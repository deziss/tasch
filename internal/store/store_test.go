package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
	bolt "go.etcd.io/bbolt"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasch.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenStampsSchemaVersion(t *testing.T) {
	s, _ := newTestStore(t)

	got, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != SchemaVersion {
		t.Errorf("schema version = %d, want %d", got, SchemaVersion)
	}
}

// TestOpenAdoptsUnversionedDatabase covers the databases written before versioning existed:
// they must still open, not be treated as corrupt.
func TestOpenAdoptsUnversionedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasch.db")

	// Build a database with the old layout: buckets, but no meta bucket.
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("bolt.Open: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketJobs, bucketGroups, bucketFairshare, bucketDeadLetters} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open rejected a pre-versioning database: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, _ := s.SchemaVersion(); got != SchemaVersion {
		t.Errorf("schema version = %d, want %d", got, SchemaVersion)
	}
}

// TestOpenRefusesNewerSchema confirms a downgrade fails loudly rather than misreading a layout
// it does not understand.
func TestOpenRefusesNewerSchema(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("bolt.Open: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte(strconv.Itoa(SchemaVersion+1)))
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("Open accepted a database written by a newer build")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("error = %v, want it to wrap ErrSchemaTooNew", err)
	}
}

func TestSaveAndLoadJobRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)

	job := &scheduler.Job{
		ID: "abc123", Command: "echo hi", Requirement: "true", State: scheduler.StateCompleted,
		User: "alice", Priority: 10, SubmitTime: time.Now().Truncate(time.Second),
		EnvVars: map[string]string{"K": "V"}, Attempt: 2,
	}
	if err := s.SaveJob(job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	got, err := s.GetJob("abc123")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Command != job.Command || got.User != job.User || got.State != job.State {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 — the fencing token must survive persistence", got.Attempt)
	}
	if got.EnvVars["K"] != "V" {
		t.Errorf("env vars = %v, want K=V", got.EnvVars)
	}
}

// TestLoadJobsSkipsCorruptRecords confirms one bad record does not block recovery of the rest.
func TestLoadJobsSkipsCorruptRecords(t *testing.T) {
	s, path := newTestStore(t)

	if err := s.SaveJob(&scheduler.Job{ID: "good", Command: "true", State: scheduler.StateQueued}); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, _ := bolt.Open(path, 0600, nil)
	_ = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJobs).Put([]byte("bad"), []byte("{not json"))
	})
	_ = db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	jobs, err := reopened.LoadJobs()
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "good" {
		t.Fatalf("LoadJobs = %d jobs, want the one readable record", len(jobs))
	}
}

func TestFairshareRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)

	want := map[string]float64{"alice": 120.5, "bob": 3}
	if err := s.SaveFairshare(want); err != nil {
		t.Fatalf("SaveFairshare: %v", err)
	}
	got, err := s.LoadFairshare()
	if err != nil {
		t.Fatalf("LoadFairshare: %v", err)
	}
	if got["alice"] != 120.5 || got["bob"] != 3 {
		t.Errorf("fairshare = %v, want %v", got, want)
	}
}

func TestDeleteJob(t *testing.T) {
	s, _ := newTestStore(t)

	if err := s.SaveJob(&scheduler.Job{ID: "gone", Command: "true"}); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	if err := s.DeleteJob("gone"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := s.GetJob("gone"); err == nil {
		t.Error("GetJob still returns a deleted job")
	}
}
