package main

import (
	"crypto/sha256"
	"sync"
	"time"
)

const idempotencyTTL = 24 * time.Hour

type idempotencyEntry struct {
	createdAt  time.Time
	bodySHA256 [32]byte
	responseID string
	inProgress bool
}

type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{entries: make(map[string]idempotencyEntry)}
}

func (s *idempotencyStore) checkAndLock(key string, body []byte) (string, *appError) {
	if key == "" {
		return "", nil
	}
	hash := sha256.Sum256(body)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	if found && now.Sub(entry.createdAt) <= idempotencyTTL {
		if entry.bodySHA256 != hash {
			return "", idempotencyReused()
		}
		if entry.inProgress {
			return "", idempotencyInProgress()
		}
		return entry.responseID, nil
	}
	s.entries[key] = idempotencyEntry{createdAt: now, bodySHA256: hash, inProgress: true}
	return "", nil
}

func (s *idempotencyStore) complete(key string, body []byte, responseID string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	s.entries[key] = idempotencyEntry{createdAt: time.Now(), bodySHA256: sha256.Sum256(body), responseID: responseID}
	s.mu.Unlock()
}

func (s *idempotencyStore) fail(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

func (s *idempotencyStore) cleanup() {
	cutoff := time.Now().Add(-idempotencyTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if entry.createdAt.Before(cutoff) {
			delete(s.entries, key)
		}
	}
}
