package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/deziss/tasch/pkg/scheduler"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketJobs        = []byte("jobs")
	bucketGroups      = []byte("groups")
	bucketFairshare   = []byte("fairshare")
	bucketDeadLetters = []byte("dead_letters")
)

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

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketJobs, bucketGroups, bucketFairshare, bucketDeadLetters} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create buckets: %w", err)
	}

	return &Store{db: db}, nil
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
		return b.ForEach(func(k, v []byte) error {
			var job scheduler.Job
			if err := json.Unmarshal(v, &job); err != nil {
				return nil // skip corrupt entries
			}
			jobs = append(jobs, &job)
			return nil
		})
	})
	return jobs, err
}

// DeleteJob removes a job from the store.
func (s *Store) DeleteJob(jobID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJobs).Delete([]byte(jobID))
	})
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
