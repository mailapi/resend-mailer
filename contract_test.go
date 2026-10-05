package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	resend "github.com/resend/resend-go/v3"
)

const validMessage = `{"from":{"email":"sender@example.com"},"to":[{"email":"to@example.com"}],"text":"Hello"}`

func contractRequest(a *app, body, key, token, prefer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	out := httptest.NewRecorder()
	a.routes().ServeHTTP(out, req)
	return out
}

type controlledMailer struct {
	started chan struct{}
	release chan struct{}
	err     *appError
	calls   int
}

func (c *controlledMailer) Send(ctx context.Context, _ *resend.SendEmailRequest, _ string) (*resend.SendEmailResponse, *appError) {
	c.calls++
	if c.started != nil {
		close(c.started)
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return &resend.SendEmailResponse{Id: "provider-id"}, nil
}

func TestAuthenticationAndAuthorizationBeforeReservation(t *testing.T) {
	a, _ := testApp()
	for _, token := range []string{"", "wrong-token"} {
		out := contractRequest(a, validMessage, "key", token, "")
		if out.Code != 401 || out.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("authentication: %v", out)
		}
	}
	a.allowedFrom = map[string]bool{"allowed@example.com": true}
	if out := contractRequest(a, validMessage, "key", "test-token", ""); out.Code != 403 {
		t.Fatalf("authorization: %v", out)
	}
	a.allowedFrom = nil
	if out := contractRequest(a, validMessage, "key", "test-token", "wait=10"); out.Code != 200 {
		t.Fatalf("key reserved before admission: %v", out)
	}
}

func TestAsyncAndTerminalReplay(t *testing.T) {
	c := &controlledMailer{started: make(chan struct{}), release: make(chan struct{})}
	a := newApp(c)
	a.token = "test-token"
	first := contractRequest(a, validMessage, "key", a.token, "")
	if first.Code != 202 || first.Header().Get("Preference-Applied") != "" {
		t.Fatalf("default: %v", first)
	}
	<-c.started
	pending := contractRequest(a, validMessage, "key", a.token, "")
	if pending.Code != 409 || pending.Header().Get("Retry-After") != "1" {
		t.Fatalf("pending: %v", pending)
	}
	close(c.release)
	a.workers.Wait()
	replay := contractRequest(a, validMessage, "key", a.token, "")
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String()[:len(first.Body.String())-1] {
		t.Fatalf("replay: %v first=%v", replay, first)
	}
	conflict := contractRequest(a, validMessage+" ", "key", a.token, "")
	if conflict.Code != 409 || c.calls != 1 {
		t.Fatalf("byte identity: %v calls=%d", conflict, c.calls)
	}
}

func TestWaitExpiryAndUnknownPreference(t *testing.T) {
	for _, prefer := range []string{"wait=0", "respond-async", "wait=bad", "wait=10000000000000000"} {
		c := &controlledMailer{release: make(chan struct{})}
		a := newApp(c)
		a.token = "test-token"
		out := contractRequest(a, validMessage, "", a.token, prefer)
		if out.Code != 202 {
			t.Fatalf("%s: %v", prefer, out)
		}
		if prefer == "wait=0" && out.Header().Get("Preference-Applied") != prefer {
			t.Fatal("missing applied preference")
		}
		if prefer != "wait=0" && out.Header().Get("Preference-Applied") != "" {
			t.Fatal("unsupported preference applied")
		}
		close(c.release)
		a.workers.Wait()
	}
}

func TestTerminalFailureIsReplayed(t *testing.T) {
	c := &controlledMailer{err: newAppError(500, "provider-error", "Provider error", "Unknown outcome")}
	a := newApp(c)
	a.token = "test-token"
	first := contractRequest(a, validMessage, "key", a.token, "wait=10")
	replay := contractRequest(a, validMessage, "key", a.token, "")
	if first.Code != 500 || replay.Code != 500 || first.Body.String() != replay.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || c.calls != 1 {
		t.Fatalf("failure: first=%v replay=%v calls=%d", first, replay, c.calls)
	}
}

func TestValidationAndExtensions(t *testing.T) {
	a, _ := testApp()
	bad := []string{
		`{"from":{"email":"sender@example.com"},"to":[{"email":"to@example.com"}]}`,
		strings.Replace(validMessage, `"text":"Hello"`, `"text":null`, 1),
		strings.Replace(validMessage, `"text":"Hello"`, `"text":"Hello","cc":[]`, 1),
		strings.Replace(validMessage, `"text":"Hello"`, `"text":"Hello","headers":[{"name":"Subject","value":"override"}]`, 1),
		strings.Replace(validMessage, `"text":"Hello"`, `"text":"Hello","headers":[{"name":"Bad:Name","value":"x"}]`, 1),
		strings.Replace(validMessage, `"text":"Hello"`, `"text":"Hello","attachments":[{"content":""}]`, 1),
	}
	for _, body := range bad {
		if out := contractRequest(a, body, "same", a.token, "wait=10"); out.Code != 422 {
			t.Fatalf("invalid message: %v body=%s", out, body)
		}
	}
	ccOnly := `{"from":{"email":"sender@example.com"},"cc":[{"email":"to@example.com"}],"text":"","extensions":{"unknown":null}}`
	if out := contractRequest(a, ccOnly, "same", a.token, "wait=10"); out.Code != 200 {
		t.Fatalf("cc-only/extensions/admission retry: %v", out)
	}
	if out := contractRequest(a, validMessage, "invalid key", a.token, ""); out.Code != 400 {
		t.Fatalf("ASCII: %v", out)
	}
}

func TestAdmissionLimitDoesNotReserveKey(t *testing.T) {
	c := &controlledMailer{release: make(chan struct{})}
	a := newApp(c)
	a.token = "test-token"
	a.queue = newAdmissionQueue(1, defaultQueueMaxBytes)
	if !a.queue.acquire(1) {
		t.Fatal("empty queue rejected")
	}
	out := contractRequest(a, validMessage, "key", a.token, "")
	if out.Code != 429 || out.Header().Get("Retry-After") != "1" {
		t.Fatalf("admission: %v", out)
	}
	a.queue.release(1)
	close(c.release)
	if out = contractRequest(a, validMessage, "key", a.token, "wait=10"); out.Code != 200 {
		t.Fatalf("reserved on 429: %v", out)
	}
}

func TestJournalRestartAndExclusiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "submissions.json")
	store, err := openIdempotencyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openIdempotencyStore(path); err == nil {
		t.Fatal("multiple writers accepted")
	}
	if _, err := store.checkAndLock("complete", []byte(validMessage)); err != nil {
		t.Fatal(err)
	}
	result := &submissionResult{Status: 200, Body: json.RawMessage(`{"id":"stable-id"}`)}
	if err := store.complete("complete", result); err != nil {
		t.Fatal(err)
	}
	if _, err := store.checkAndLock("interrupted", []byte(validMessage)); err != nil {
		t.Fatal(err)
	}
	store.close()
	reopened, err := openIdempotencyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	replay, problem := reopened.checkAndLock("complete", []byte(validMessage))
	if problem != nil || replay.Status != 200 || string(replay.Body) != string(result.Body) {
		t.Fatalf("terminal persistence: %v %v", replay, problem)
	}
	interrupted, problem := reopened.checkAndLock("interrupted", []byte(validMessage))
	if problem != nil || interrupted.Status != 500 {
		t.Fatalf("interrupted submission reexecuted: %v %v", interrupted, problem)
	}
}

func TestPrincipalScopeAndStrictRequestSyntax(t *testing.T) {
	a, mock := testApp()
	first := contractRequest(a, validMessage, "key", a.token, "wait=10")
	a.token = "another-token"
	second := contractRequest(a, validMessage, "key", a.token, "wait=10")
	if first.Code != 200 || second.Code != 200 || first.Body.String() == second.Body.String() {
		t.Fatalf("principal scope: %v %v", first, second)
	}
	mock.mu.Lock()
	if len(mock.sentEmails) != 2 || mock.sentEmails[0].IdempotencyKey == mock.sentEmails[1].IdempotencyKey {
		t.Fatal("downstream key namespace collided")
	}
	mock.mu.Unlock()
	for _, body := range []string{
		strings.Replace(validMessage, `"text":"Hello"`, `"text":"Hello","TEXT":"other"`, 1),
		strings.Replace(validMessage, `"email":"sender@example.com"`, `"email":"sender@example.com","Email":"other@example.com"`, 1),
		strings.Replace(validMessage, "Hello", string([]byte{0xff}), 1),
	} {
		if out := contractRequest(a, body, "fresh-key", a.token, ""); out.Code != 400 {
			t.Fatalf("invalid syntax: %v", out)
		}
	}
}

func TestJournalAdmissionFailureDoesNotDispatch(t *testing.T) {
	a, mock := testApp()
	a.idempotency.path = filepath.Join(t.TempDir(), "missing", "journal.json")
	if out := contractRequest(a, validMessage, "key", a.token, ""); out.Code != 503 {
		t.Fatalf("unavailable journal: %v", out)
	}
	a.idempotency.path = ""
	if out := contractRequest(a, validMessage, "key", a.token, "wait=10"); out.Code != 200 {
		t.Fatalf("reserved failed admission: %v", out)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.sentEmails) != 1 {
		t.Fatalf("dispatched %d times", len(mock.sentEmails))
	}
}

func TestStablePrincipalSurvivesTokenRotation(t *testing.T) {
	mailer := &controlledMailer{}
	a := newApp(mailer)
	a.token = "test-token"
	a.principal = "wiki"
	first := contractRequest(a, validMessage, "rotation-key", a.token, "wait=10")
	a.token = "rotated-token"
	replay := contractRequest(a, validMessage, "rotation-key", a.token, "wait=10")
	if first.Code != 200 || replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != first.Body.String() || mailer.calls != 1 {
		t.Fatalf("first=%v replay=%v calls=%d", first, replay, mailer.calls)
	}
}

func TestJournalReadinessAndExpiredPendingCleanup(t *testing.T) {
	a, _ := testApp()
	directory := t.TempDir()
	a.idempotency.path = filepath.Join(directory, "state.json")
	out := httptest.NewRecorder()
	a.routes().ServeHTTP(out, httptest.NewRequest("GET", "/ready", nil))
	if out.Code != 200 {
		t.Fatalf("writable readiness: %v", out)
	}
	a.idempotency.entries["expired"] = idempotencyEntry{CreatedAt: time.Now().Add(-25 * time.Hour)}
	a.idempotency.entries["current"] = idempotencyEntry{CreatedAt: time.Now()}
	a.idempotency.cleanup()
	if len(a.idempotency.entries) != 1 {
		t.Fatalf("entries: %v", a.idempotency.entries)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	out = httptest.NewRecorder()
	a.routes().ServeHTTP(out, httptest.NewRequest("GET", "/ready", nil))
	if out.Code != 503 {
		t.Fatalf("unwritable readiness: %v", out)
	}
	out = httptest.NewRecorder()
	a.routes().ServeHTTP(out, httptest.NewRequest("GET", "/health", nil))
	if out.Code != 200 {
		t.Fatalf("liveness: %v", out)
	}
}

type journalFailureMailer struct{ store *idempotencyStore }

func (m journalFailureMailer) Send(context.Context, *resend.SendEmailRequest, string) (*resend.SendEmailResponse, *appError) {
	m.store.path = filepath.Join(m.store.path, "missing", "state.json")
	return &resend.SendEmailResponse{Id: "sent-successfully"}, nil
}

func TestTerminalPersistenceFailurePreservesActualSuccess(t *testing.T) {
	store := newIdempotencyStore()
	store.path = filepath.Join(t.TempDir(), "state.json")
	a := newApp(journalFailureMailer{store})
	a.token = "test-token"
	a.idempotency = store
	first := contractRequest(a, validMessage, "persist-key", a.token, "wait=10")
	replay := contractRequest(a, validMessage, "persist-key", a.token, "wait=10")
	if first.Code != 200 || replay.Code != 200 || replay.Body.String() != first.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("first=%v replay=%v", first, replay)
	}
}

func TestSequentialSubmissionsQueueWhileWorkersSend(t *testing.T) {
	c := &blockingMailer{release: make(chan struct{})}
	a := newApp(c)
	a.token = "test-token"
	// Two dispatch slots are busy; later submissions queue instead of failing.
	for i := 0; i < defaultQueueLimit; i++ {
		if out := contractRequest(a, validMessage, "", a.token, ""); out.Code != 202 {
			t.Fatalf("submission %d: %v", i, out)
		}
	}
	if out := contractRequest(a, validMessage, "", a.token, ""); out.Code != 429 {
		t.Fatalf("full queue: %v", out)
	}
	close(c.release)
	a.workers.Wait()
	if got := c.sent.Load(); got != defaultQueueLimit {
		t.Fatalf("sent %d of %d queued submissions", got, defaultQueueLimit)
	}
	if out := contractRequest(a, validMessage, "", a.token, "wait=10"); out.Code != 200 {
		t.Fatalf("drained queue: %v", out)
	}
}

func TestQueueByteBudget(t *testing.T) {
	q := newAdmissionQueue(10, 100)
	if !q.acquire(150) {
		t.Fatal("oversized request rejected from an empty queue")
	}
	if q.acquire(1) {
		t.Fatal("byte budget exceeded")
	}
	q.release(150)
	if !q.acquire(60) || !q.acquire(40) || q.acquire(1) {
		t.Fatal("byte budget accounting")
	}
}

type blockingMailer struct {
	release chan struct{}
	sent    atomic.Int64
}

func (m *blockingMailer) Send(ctx context.Context, _ *resend.SendEmailRequest, _ string) (*resend.SendEmailResponse, *appError) {
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	m.sent.Add(1)
	return &resend.SendEmailResponse{Id: "provider-id"}, nil
}
