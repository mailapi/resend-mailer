package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

// resend-go encodes attachment bytes as an integer array, so this conservative
// limit keeps peak attachment serialization memory below the pod memory limit.
const maxBodyLimitBytes int64 = 10 * 1024 * 1024

type app struct {
	mailer      mailerClient
	idempotency *idempotencyStore
	maxBodySize int64
}

func newApp(mailer mailerClient) *app {
	return &app{mailer: mailer, idempotency: newIdempotencyStore(), maxBodySize: maxBodyLimitBytes}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /v1/health", healthHandler)
	mux.HandleFunc("POST /v1/messages", a.createMessageHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK")
}

func (a *app) createMessageHandler(w http.ResponseWriter, r *http.Request) {
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
	if len(key) > 256 {
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

	cachedID, lockErr := a.idempotency.checkAndLock(key, body)
	if lockErr != nil {
		writeProblem(w, lockErr)
		return
	}
	if cachedID != "" {
		writeJSON(w, http.StatusOK, MessageAcceptedResponse{ID: cachedID})
		return
	}
	failed := true
	defer func() {
		if failed {
			a.idempotency.fail(key)
		}
	}()

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

	email, buildErr := buildResendEmail(&request)
	if buildErr != nil {
		writeProblem(w, buildErr)
		return
	}
	response, sendErr := a.mailer.Send(r.Context(), email, key)
	if sendErr != nil {
		writeProblem(w, sendErr)
		return
	}

	failed = false
	a.idempotency.complete(key, body, response.Id)
	slog.Info("Message accepted and forwarded to Resend", "id", response.Id)
	writeJSON(w, http.StatusOK, MessageAcceptedResponse{ID: response.Id})
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
