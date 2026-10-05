package main

import (
	"encoding/json"
	"net/http"
)

type appError struct {
	status     int
	problem    Problem
	retryAfter string
}

func (e *appError) Error() string { return e.problem.Detail }

func newAppError(status int, kind, title, detail string) *appError {
	return &appError{status: status, problem: Problem{
		Type: "https://mailapi.github.io/problems/" + kind, Title: title, Status: status, Detail: detail,
	}}
}

func badRequest(detail string) *appError {
	return newAppError(http.StatusBadRequest, "malformed-request", "Bad Request", detail)
}

func unprocessable(detail string) *appError {
	return newAppError(http.StatusUnprocessableEntity, "invalid-message", "Unprocessable Entity", detail)
}

func idempotencyReused() *appError {
	return newAppError(http.StatusConflict, "idempotency-key-reused", "Idempotency key reused", "This key was already used with a different request body.")
}

func idempotencyInProgress() *appError {
	err := newAppError(http.StatusConflict, "idempotency-key-in-progress", "Idempotent submission in progress", "Retry the request with the same key later.")
	err.retryAfter = "1"
	return err
}

func writeProblem(w http.ResponseWriter, err *appError) {
	w.Header().Set("Content-Type", "application/problem+json")
	if err.retryAfter != "" {
		w.Header().Set("Retry-After", err.retryAfter)
	}
	w.WriteHeader(err.status)
	_ = json.NewEncoder(w).Encode(err.problem)
}

func problemResult(err *appError) *submissionResult {
	body, _ := json.Marshal(err.problem)
	return &submissionResult{Status: err.status, Body: body}
}

func writeResult(w http.ResponseWriter, result *submissionResult) {
	contentType := "application/json"
	if result.Status >= 400 {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}
