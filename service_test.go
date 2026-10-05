package main

import (
	"context"
	"errors"
	resend "github.com/resend/resend-go/v3"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMapProviderErrorPreservesStatusAndRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		metadata   providerResponseMetadata
		wantStatus int
		wantType   string
		wantRetry  string
	}{
		{"idempotency conflict", providerResponseMetadata{status: 409, name: "invalid_idempotent_request", message: "key reused"}, 409, "https://mailapi.github.io/problems/idempotency-key-reused", ""},
		{"provider unavailable", providerResponseMetadata{status: 503, message: "maintenance", retryAfter: "30"}, 503, "https://mailapi.github.io/problems/provider-unavailable", "30"},
		{"rate limited", providerResponseMetadata{status: 429, message: "slow down", retryAfter: "5"}, 429, "https://mailapi.github.io/problems/rate-limit-exceeded", "5"},
		{"authentication failure is not validation", providerResponseMetadata{status: 401, message: "invalid API key"}, 500, "https://mailapi.github.io/problems/provider-error", ""},
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

func TestProviderTemporaryRejectionsRetryWithSameKey(t *testing.T) {
	for _, status := range []int{429, 503, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Idempotency-Key") != "same-key" {
					t.Errorf("unexpected key: %s", r.Header.Get("Idempotency-Key"))
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					io.WriteString(w, `{"name":"temporary_error","message":"rejected"}`)
					return
				}
				io.WriteString(w, `{"id":"provider-id"}`)
			}))
			defer server.Close()
			client := resend.NewCustomClient(&http.Client{Transport: responseCapturingTransport{next: http.DefaultTransport}}, "test")
			client.BaseURL, _ = url.Parse(server.URL + "/")
			mailer := &resendMailerClient{client: client}
			response, err := mailer.Send(context.Background(), &resend.SendEmailRequest{From: "from@example.com", To: []string{"to@example.com"}, Subject: "Test", Text: "Hello"}, "same-key")
			if status == 422 {
				if calls != 1 || err == nil {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				return
			}
			if calls != 2 || err != nil || response.Id != "provider-id" {
				t.Fatalf("calls=%d response=%v error=%v", calls, response, err)
			}
		})
	}
}

func TestProviderPacingAndCancellation(t *testing.T) {
	client := &resendMailerClient{interval: 30 * time.Millisecond}
	if err := client.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := client.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("provider requests were not paced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.admit(ctx); err == nil {
		t.Fatal("canceled wait admitted")
	}
}

func TestCanceledPacingReturnsSlot(t *testing.T) {
	client := &resendMailerClient{interval: 100 * time.Millisecond}
	if err := client.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.admit(ctx); err == nil {
		t.Fatal("canceled wait admitted")
	}
	// Without the refund, the canceled reservation would push this to ~200ms.
	start := time.Now()
	if err := client.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 170*time.Millisecond {
		t.Fatalf("canceled reservation was not returned: waited %v", elapsed)
	}
}

func TestProviderRetryBoundsAndGeneratedKeys(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		deadline   time.Duration
		wantCalls  int
	}{
		{"unkeyed stable retry", "0", time.Minute, 2},
		{"long retry after", "30", time.Minute, 1},
		{"insufficient deadline", "0", time.Second, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			firstKey, firstBody := "", ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				key := r.Header.Get("Idempotency-Key")
				body, _ := io.ReadAll(r.Body)
				if key == "" {
					t.Error("provider key missing")
				}
				if calls == 1 {
					firstKey, firstBody = key, string(body)
				} else if key != firstKey || string(body) != firstBody {
					t.Error("retry changed key or body")
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.Header().Set("Retry-After", test.retryAfter)
					w.WriteHeader(503)
					io.WriteString(w, `{"name":"service_unavailable","message":"temporary"}`)
					return
				}
				io.WriteString(w, `{"id":"provider-id"}`)
			}))
			defer server.Close()
			client := resend.NewCustomClient(&http.Client{Transport: responseCapturingTransport{next: http.DefaultTransport}}, "test")
			client.BaseURL, _ = url.Parse(server.URL + "/")
			ctx, cancel := context.WithTimeout(context.Background(), test.deadline)
			defer cancel()
			_, err := (&resendMailerClient{client: client}).Send(ctx, &resend.SendEmailRequest{From: "from@example.com", To: []string{"to@example.com"}, Text: "Hello"}, "")
			if calls != test.wantCalls || (err == nil) != (test.wantCalls == 2) {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestProviderRetryAfterParsing(t *testing.T) {
	for _, value := range []string{"30", "60", "-1", "invalid", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)} {
		if _, ok := providerRetryDelay(value); ok {
			t.Fatalf("accepted long/invalid delay %q", value)
		}
	}
	for _, value := range []string{"0", "1", "2", time.Now().Add(time.Second).UTC().Format(http.TimeFormat)} {
		delay, ok := providerRetryDelay(value)
		if !ok || delay < 0 || delay > 2*time.Second {
			t.Fatalf("delay=%v ok=%v value=%q", delay, ok, value)
		}
	}
}
