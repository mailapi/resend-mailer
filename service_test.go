package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMapProviderErrorPreservesStatusAndRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		metadata   providerResponseMetadata
		wantStatus int
		wantType   string
		wantRetry  string
	}{
		{"idempotency conflict", providerResponseMetadata{status: 409, name: "invalid_idempotent_request", message: "key reused"}, 409, "https://api.example.com/problems/idempotency-key-reused", ""},
		{"provider unavailable", providerResponseMetadata{status: 503, message: "maintenance", retryAfter: "30"}, 503, "https://api.example.com/problems/service-unavailable", "30"},
		{"rate limited", providerResponseMetadata{status: 429, message: "slow down", retryAfter: "5"}, 429, "https://api.example.com/problems/rate-limit-exceeded", "5"},
		{"authentication failure is not validation", providerResponseMetadata{status: 401, message: "invalid API key"}, 500, "https://api.example.com/problems/internal-server-error", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := mapProviderError(&test.metadata, errors.New("provider error"))
			if err.status != test.wantStatus || err.problem.Type != test.wantType || err.retryAfter != test.wantRetry {
				t.Fatalf("error=%+v", err)
			}
		})
	}
}

func TestResponseCapturingTransportRetainsProviderResponse(t *testing.T) {
	metadata := &providerResponseMetadata{}
	request, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(context.WithValue(request.Context(), providerResponseMetadataContextKey{}, metadata))
	transport := responseCapturingTransport{next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": []string{"12"}}, Body: io.NopCloser(strings.NewReader(`{"name":"service_unavailable","message":"maintenance"}`))}, nil
	})}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if metadata.status != 503 || metadata.retryAfter != "12" || metadata.message != "maintenance" || string(body) != `{"name":"service_unavailable","message":"maintenance"}` {
		t.Fatalf("metadata=%+v body=%s", metadata, body)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
