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
		Type: "https://api.example.com/problems/" + kind, Title: title, Status: status, Detail: detail,
	}}
}

func badRequest(detail string) *appError {
	return newAppError(http.StatusBadRequest, "bad-request", "Bad Request", detail)
}

func unprocessable(detail string) *appError {
	return newAppError(http.StatusUnprocessableEntity, "unprocessable-entity", "Unprocessable Entity", detail)
}

func idempotencyReused() *appError {
	return newAppError(http.StatusConflict, "idempotency-key-reused", "Idempotency key reused", "This key was already used with a different request body.")
}

func idempotencyInProgress() *appError {
	return newAppError(http.StatusConflict, "idempotency-key-in-progress", "Idempotent submission in progress", "Retry the request with the same key later.")
}

func writeProblem(w http.ResponseWriter, err *appError) {
	w.Header().Set("Content-Type", "application/problem+json")
	if err.retryAfter != "" {
		w.Header().Set("Retry-After", err.retryAfter)
	}
	w.WriteHeader(err.status)
	_ = json.NewEncoder(w).Encode(err.problem)
}
