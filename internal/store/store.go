package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/deziss/tasch/pkg/scheduler"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

var (
	bucketJobs        = []byte("jobs")
	bucketGroups      = []byte("groups")
	bucketFairshare   = []byte("fairshare")
	bucketDeadLetters = []byte("dead_letters")
	bucketMeta        = []byte("meta")
	bucketCordons     = []byte("cordons")
)

// SchemaVersion is the on-disk format this build writes.
//
// The database previously carried no version at all, so an upgrade that changed a persisted
// struct simply failed to unmarshal — and LoadJobs discarded unmarshal errors silently, making
// jobs vanish with no log line and no metric. Recording the version lets a future change detect
// what it is reading and refuse rather than corrupt.
const SchemaVersion = 1

var keySchemaVersion = []byte("schema_version")

// ErrSchemaTooNew reports a database written by a newer build.
var ErrSchemaTooNew = errors.New("database schema is newer than this build supports")

// Store provides BoltDB persistence for jobs, groups, and fairshare data.
type Store struct {
	db *bolt.DB
}

// Open creates or opens a BoltDB store at the given path.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Create buckets and establish the schema version.
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketJobs, bucketGroups, bucketFairshare, bucketDeadLetters, bucketMeta, bucketCordons} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}

		meta := tx.Bucket(bucketMeta)
		raw := meta.Get(keySchemaVersion)
		if raw == nil {
			// Either a fresh database or one written before versioning existed. Both are
			// readable as version 1, so stamp it and carry on.
			return meta.Put(keySchemaVersion, []byte(strconv.Itoa(SchemaVersion)))
		}

		found, convErr := strconv.Atoi(string(raw))
		if convErr != nil {
			return fmt.Errorf("database schema version %q is not a number", raw)
		}
		if found > SchemaVersion {
			// Refuse rather than silently misread a newer layout, which is how a downgrade
			// would otherwise lose data.
			return fmt.Errorf("%w: database is version %d, this build supports %d",
				ErrSchemaTooNew, found, SchemaVersion)
		}
		if found < SchemaVersion {
			if migrateErr := migrate(tx, found, SchemaVersion); migrateErr != nil {
				return migrateErr
			}
			return meta.Put(keySchemaVersion, []byte(strconv.Itoa(SchemaVersion)))
		}
		return nil
	})
	if err != nil {
		func() { _ = db.Close() }()
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// migrate upgrades the on-disk layout from one version to the next.
//
// Version 1 is the first recorded schema and nothing predates it — an unversioned database is
// adopted as version 1 by Open — so there is no migration path to implement yet. This exists so
// the next schema change has an obvious place to go, and so an unexpected older version fails
// loudly instead of being read with the wrong layout.
func migrate(tx *bolt.Tx, from, to int) error {
	return fmt.Errorf("no migration path from schema version %d to %d", from, to)
}

// SchemaVersion returns the version recorded in the database.
func (s *Store) SchemaVersion() (int, error) {
	var version int
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if raw == nil {
			return errors.New("no schema version recorded")
		}
		v, err := strconv.Atoi(string(raw))
		if err != nil {
			return err
		}
		version = v
		return nil
	})
	return version, err
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// --- Jobs ---

// SaveJob persists a job to the store.
func (s *Store) SaveJob(job *scheduler.Job) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		data, err := json.Marshal(job)
		if err != nil {
			return err
		}
		return b.Put([]byte(job.ID), data)
	})
}

// LoadJobs loads all jobs from the store.
func (s *Store) LoadJobs() ([]*scheduler.Job, error) {
	var jobs []*scheduler.Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		corrupt := 0
		if err := b.ForEach(func(k, v []byte) error {
			var job scheduler.Job
			if err := json.Unmarshal(v, &job); err != nil {
				// Keep going — one bad record must not block recovery of the rest — but say so.
				// These errors used to be discarded, so a struct change made jobs disappear with
				// no diagnostic anywhere.
				corrupt++
				log.Printf("[store] Skipping unreadable job record %q: %v", k, err)
				return nil
			}
			jobs = append(jobs, &job)
			return nil
		}); err != nil {
			return err
		}
		if corrupt > 0 {
			log.Printf("[store] %d job record(s) could not be read and were skipped", corrupt)
		}
		return nil
	})
	return jobs, err
}

// DeleteJob removes a job from the store.
func (s *Store) DeleteJob(jobID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJobs).Delete([]byte(jobID))
	})
}

// GetJob retrieves a single job from the store by ID.
func (s *Store) GetJob(jobID string) (*scheduler.Job, error) {
	var job scheduler.Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		v := b.Get([]byte(jobID))
		if v == nil {
			return fmt.Errorf("job not found")
		}
		return json.Unmarshal(v, &job)
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// --- Groups ---

// SaveGroup persists a job group.
func (s *Store) SaveGroup(group *scheduler.JobGroup) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		data, err := json.Marshal(group)
		if err != nil {
			return err
		}
		return b.Put([]byte(group.GroupID), data)
	})
}

// LoadGroups loads all groups from the store.
func (s *Store) LoadGroups() ([]*scheduler.JobGroup, error) {
	var groups []*scheduler.JobGroup
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		return b.ForEach(func(k, v []byte) error {
			var g scheduler.JobGroup
			if err := json.Unmarshal(v, &g); err != nil {
				return nil
			}
			groups = append(groups, &g)
			return nil
		})
	})
	return groups, err
}

// --- Fairshare ---

// SaveFairshare persists fairshare usage data.
func (s *Store) SaveFairshare(usage map[string]float64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFairshare)
		data, err := json.Marshal(usage)
		if err != nil {
			return err
		}
		return b.Put([]byte("usage"), data)
	})
}

// LoadFairshare loads fairshare usage data.
func (s *Store) LoadFairshare() (map[string]float64, error) {
	result := make(map[string]float64)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFairshare)
		data := b.Get([]byte("usage"))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &result)
	})
	return result, err
}

// --- Dead Letters ---

// SaveDeadLetter archives a job that exhausted all retries.
func (s *Store) SaveDeadLetter(job *scheduler.Job) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDeadLetters)
		data, err := json.Marshal(job)
		if err != nil {
			return err
		}
		return b.Put([]byte(job.ID), data)
	})
}

// LoadDeadLetters returns all dead letter jobs.
func (s *Store) LoadDeadLetters() ([]*scheduler.Job, error) {
	var jobs []*scheduler.Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDeadLetters)
		return b.ForEach(func(k, v []byte) error {
			var job scheduler.Job
			if err := json.Unmarshal(v, &job); err != nil {
				return nil
			}
			jobs = append(jobs, &job)
			return nil
		})
	})
	return jobs, err
}

// GetDeadLetter retrieves a single dead letter job from the store by ID.
func (s *Store) GetDeadLetter(jobID string) (*scheduler.Job, error) {
	var job scheduler.Job
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDeadLetters)
		v := b.Get([]byte(jobID))
		if v == nil {
			return fmt.Errorf("dead letter not found")
		}
		return json.Unmarshal(v, &job)
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// --- Cordons ---

// SaveCordons persists the set of nodes taken out of scheduling rotation.
//
// These must survive a master restart. A node cordoned for maintenance that silently returns to
// service because the master was restarted is worse than not having cordoned it at all.
func (s *Store) SaveCordons(entries map[string][]byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		// Replace wholesale so an uncordon is not left behind as a stale entry.
		if err := tx.DeleteBucket(bucketCordons); err != nil && !errors.Is(err, bolterrors.ErrBucketNotFound) {
			return err
		}
		b, err := tx.CreateBucket(bucketCordons)
		if err != nil {
			return err
		}
		for node, data := range entries {
			if err := b.Put([]byte(node), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadCordons reads the persisted cordon set.
func (s *Store) LoadCordons() (map[string][]byte, error) {
	out := make(map[string][]byte)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCordons)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			// Bolt's values are only valid for the life of the transaction.
			data := make([]byte, len(v))
			copy(data, v)
			out[string(k)] = data
			return nil
		})
	})
	return out, err
}
