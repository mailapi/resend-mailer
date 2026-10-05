package main

import "sync"

const (
	defaultQueueLimit    = 20
	defaultQueueMaxBytes = 32 * 1024 * 1024
)

// admissionQueue bounds accepted submissions that have not finished, by count
// and request bytes. Dispatch concurrency is bounded separately by app.slots,
// so sequential clients are queued instead of rejected while workers send.
type admissionQueue struct {
	mu       sync.Mutex
	count    int
	bytes    int64
	maxCount int
	maxBytes int64
}

func newAdmissionQueue(maxCount int, maxBytes int64) *admissionQueue {
	return &admissionQueue{maxCount: maxCount, maxBytes: maxBytes}
}

func (q *admissionQueue) acquire(size int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	// A single request is always admitted into an empty queue, so a limit
	// smaller than the body limit cannot block a valid request forever.
	if q.count >= q.maxCount || (q.count > 0 && q.bytes+size > q.maxBytes) {
		return false
	}
	q.count++
	q.bytes += size
	return true
}

func (q *admissionQueue) release(size int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.count--
	q.bytes -= size
}
