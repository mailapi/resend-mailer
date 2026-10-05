package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const idempotencyTTL = 24 * time.Hour

type submissionResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type idempotencyEntry struct {
	CreatedAt  time.Time         `json:"createdAt"`
	BodySHA256 [32]byte          `json:"bodySHA256"`
	Result     *submissionResult `json:"result,omitempty"`
}

type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
	path    string
	lock    *os.File
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{entries: make(map[string]idempotencyEntry)}
}

// A single process owns the journal. Pending submissions recovered after a
// crash have an unknown outcome and must never be dispatched again.
func openIdempotencyStore(path string) (*idempotencyStore, error) {
	s := newIdempotencyStore()
	s.path = path
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, err
	}
	s.lock = lock
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		s.close()
		return nil, err
	}
	if len(body) > 0 {
		if err = json.Unmarshal(body, &s.entries); err != nil {
			s.close()
			return nil, err
		}
	}
	if s.entries == nil {
		s.entries = make(map[string]idempotencyEntry)
	}
	for key, entry := range s.entries {
		if time.Since(entry.CreatedAt) > idempotencyTTL {
			delete(s.entries, key)
			continue
		}
		if entry.Result == nil {
			entry.Result = problemResult(newAppError(500, "provider-error", "Provider error", "Submission was interrupted; its outcome is unknown."))
			s.entries[key] = entry
		}
	}
	if err = s.persist(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *idempotencyStore) close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
	}
}

// Called under mu, or before the store is shared. Atomic replacement prevents
// readers after a restart from seeing a partially written journal.
func (s *idempotencyStore) persist() error {
	if s.path == "" {
		return nil
	}
	body, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".submissions-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *idempotencyStore) checkAndLock(key string, body []byte) (*submissionResult, *appError) {
	if key == "" {
		return nil, nil
	}
	hash := sha256.Sum256(body)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	if found && now.Sub(entry.CreatedAt) <= idempotencyTTL {
		if entry.BodySHA256 != hash {
			return nil, idempotencyReused()
		}
		if entry.Result == nil {
			return nil, idempotencyInProgress()
		}
		return entry.Result, nil
	}
	s.entries[key] = idempotencyEntry{CreatedAt: now, BodySHA256: hash}
	if err := s.persist(); err != nil {
		delete(s.entries, key)
		return nil, newAppError(503, "provider-unavailable", "Provider unavailable", "Unable to reserve submission; execution did not begin.")
	}
	return nil, nil
}

func (s *idempotencyStore) complete(key string, result *submissionResult) error {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	entry.Result = result
	s.entries[key] = entry
	return s.persist()
}

func (s *idempotencyStore) cleanup() {
	cutoff := time.Now().Add(-idempotencyTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if entry.CreatedAt.Before(cutoff) {
			delete(s.entries, key)
		}
	}
	_ = s.persist()
}

func (s *idempotencyStore) probe() error {
	if s.path == "" {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".readiness-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write([]byte("ready")); err != nil {
		return err
	}
	return file.Sync()
}
