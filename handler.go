package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// resend-go encodes attachment bytes as an integer array, so this conservative
// limit keeps peak attachment serialization memory below the pod memory limit.
const maxBodyLimitBytes int64 = 10 * 1024 * 1024

type app struct {
	mailer      mailerClient
	idempotency *idempotencyStore
	maxBodySize int64
	token       string
	principal   string
	allowedFrom map[string]bool
	workers     sync.WaitGroup
	slots       chan struct{}
	queue       *admissionQueue
}

func newApp(mailer mailerClient) *app {
	return &app{mailer: mailer, idempotency: newIdempotencyStore(), maxBodySize: maxBodyLimitBytes, slots: make(chan struct{}, 2), queue: newAdmissionQueue(defaultQueueLimit, defaultQueueMaxBytes)}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /v1/health", healthHandler)
	mux.HandleFunc("GET /ready", a.readinessHandler)
	mux.HandleFunc("POST /v1/messages", a.createMessageHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK")
}

func (a *app) createMessageHandler(w http.ResponseWriter, r *http.Request) {
	credentials := strings.Fields(r.Header.Get("Authorization"))
	if len(r.Header.Values("Authorization")) != 1 || a.token == "" || len(credentials) != 2 || !strings.EqualFold(credentials[0], "Bearer") || subtle.ConstantTimeCompare([]byte(credentials[1]), []byte(a.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mailapi"`)
		writeProblem(w, newAppError(401, "unauthenticated", "Unauthenticated", "Missing or invalid bearer token."))
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		detail := "Missing Content-Type header. Expected 'application/json'"
		if value := r.Header.Get("Content-Type"); value != "" {
			detail = fmt.Sprintf("Unsupported Content-Type: '%s'. Expected 'application/json'", value)
		}
		writeProblem(w, newAppError(http.StatusUnsupportedMediaType, "unsupported-media-type", "Unsupported Media Type", detail))
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) > 1 || len(key) > 256 || (key != "" && !visibleASCII(key)) {
		writeProblem(w, badRequest("Idempotency-Key must be between 1 and 256 characters"))
		return
	}
	if _, present := r.Header[http.CanonicalHeaderKey("Idempotency-Key")]; present && key == "" {
		writeProblem(w, badRequest("Idempotency-Key must be between 1 and 256 characters"))
		return
	}

	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, a.maxBodySize))
	if readErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			writeProblem(w, newAppError(http.StatusRequestEntityTooLarge, "payload-too-large", "Payload Too Large", fmt.Sprintf("Request body exceeds maximum allowed limit: %v", readErr)))
		} else {
			writeProblem(w, badRequest(readErr.Error()))
		}
		return
	}

	if !utf8.Valid(body) {
		writeProblem(w, badRequest("Request body must be valid UTF-8"))
		return
	}

	var request OutboundMessageRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeProblem(w, badRequest("Malformed JSON: "+err.Error()))
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeProblem(w, badRequest("Malformed JSON: "+err.Error()))
		return
	}

	if validationErr := validateJSONFields(body); validationErr != nil {
		writeProblem(w, validationErr)
		return
	}
	if len(a.allowedFrom) > 0 && !a.allowedFrom[strings.ToLower(request.From.Email)] {
		writeProblem(w, newAppError(403, "forbidden", "Forbidden", "This principal may not send as the requested from address."))
		return
	}
	email, buildErr := buildResendEmail(&request)
	if buildErr != nil {
		writeProblem(w, buildErr)
		return
	}
	// Admission happens before a key is reserved. No dispatch has begun on 429.
	size := int64(len(body))
	if !a.queue.acquire(size) {
		admissionErr := newAppError(429, "rate-limit-exceeded", "Rate limit exceeded", "Submission queue is full.")
		admissionErr.retryAfter = "1"
		writeProblem(w, admissionErr)
		return
	}
	dispatched := false
	defer func() {
		if !dispatched {
			a.queue.release(size)
		}
	}()
	// Scope both local and downstream keys to this authenticated principal.
	scopedKey := ""
	if key != "" {
		scopedKey = fmt.Sprintf("%x", sha256.Sum256([]byte(a.principalID()+"\x00"+key)))
	}
	cached, lockErr := a.idempotency.checkAndLock(scopedKey, body)
	if lockErr != nil {
		writeProblem(w, lockErr)
		return
	}
	if cached != nil {
		w.Header().Set("Idempotency-Replayed", "true")
		writeResult(w, cached)
		return
	}
	id := "msg_" + rand.Text()
	accepted := MessageAcceptedResponse{ID: id}
	done := make(chan *submissionResult, 1)
	dispatched = true
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		defer a.queue.release(size)
		// Queued submissions wait here for a dispatch slot; the dispatch
		// deadline starts only once sending begins.
		a.slots <- struct{}{}
		defer func() { <-a.slots }()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
		defer cancel()
		response, sendErr := a.mailer.Send(ctx, email, scopedKey)
		var result *submissionResult
		if sendErr != nil {
			// Downstream dispatch already began. Its failure is a terminal outcome,
			// not a local admission error that could safely be executed again.
			result = problemResult(newAppError(500, "provider-error", "Provider error", sendErr.problem.Detail))
		} else if response == nil || response.Id == "" {
			result = problemResult(newAppError(500, "provider-error", "Provider error", "Provider returned no message identifier."))
		} else {
			encoded, _ := json.Marshal(accepted)
			result = &submissionResult{Status: 200, Body: encoded}
			slog.Info("Submission completed", "id", id, "provider_id", response.Id)
		}
		if err := a.idempotency.complete(scopedKey, result); err != nil {
			slog.Error("Unable to persist terminal submission", "id", id, "error", err)
			// Keep the actual outcome in memory. A pending disk entry recovers
			// conservatively as 500 if the process restarts before storage is repaired.
		}
		done <- result
	}()
	wait, applied := waitPreference(r.Header.Values("Prefer"))
	if applied {
		w.Header().Set("Preference-Applied", fmt.Sprintf("wait=%d", wait))
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case result := <-done:
			writeResult(w, result)
			return
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusAccepted, accepted)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func visibleASCII(value string) bool {
	for _, c := range value {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// Ignore unsupported preferences. Bound accepted waits to the dispatch timeout.
func waitPreference(values []string) (int, bool) {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			name, raw, found := strings.Cut(strings.TrimSpace(part), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "wait") {
				continue
			}
			raw = strings.Trim(strings.TrimSpace(raw), `"`)
			wait, err := strconv.Atoi(raw)
			if err == nil && wait >= 0 && wait <= 60 {
				return wait, true
			}
		}
	}
	return 0, false
}

func (a *app) principalID() string {
	if a.principal != "" {
		return a.principal
	}
	return a.token // Preserve v0.3.0 namespaces until a stable principal is configured.
}

func (a *app) readinessHandler(w http.ResponseWriter, r *http.Request) {
	if err := a.idempotency.probe(); err != nil {
		writeProblem(w, newAppError(503, "provider-unavailable", "Provider unavailable", "Submission journal is not writable."))
		return
	}
	healthHandler(w, r)
}
