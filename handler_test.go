package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testApp() (*app, *mockMailerClient) {
	mock := &mockMailerClient{}
	return newApp(mock), mock
}

func request(t *testing.T, handler http.Handler, method, path, contentType, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestHealth(t *testing.T) {
	a, _ := testApp()
	for _, path := range []string{"/health", "/v1/health"} {
		response := request(t, a.routes(), http.MethodGet, path, "", "", "")
		if response.Code != http.StatusOK || response.Body.String() != "OK" {
			t.Fatalf("%s: code=%d body=%q", path, response.Code, response.Body.String())
		}
	}
}

func TestSendMessageAllFields(t *testing.T) {
	a, mock := testApp()
	payload := `{
      "from":{"email":"sender@example.com","name":"Alice"},
      "to":[{"email":"to1@example.com","name":"Bob"},{"email":"to2@example.com"}],
      "cc":[{"email":"cc1@example.com","name":"Charlie"}],
      "bcc":[{"email":"bcc1@example.com"}],
      "replyTo":[{"email":"reply@example.com","name":"Support"}],
      "subject":"Complete Email Test","text":"Plain text body","html":"<h1>HTML Body</h1>",
	  "headers":[{"name":"X-Tracking","value":"one"},{"name":"X-Entity-ID","value":"two"}],
      "attachments":[{"filename":"document.txt","contentType":"text/plain","content":"` + base64.StdEncoding.EncodeToString([]byte("Hello attachment")) + `"}]
    }`
	response := request(t, a.routes(), http.MethodPost, "/v1/messages", "application/json", payload, "key-1")
	if response.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var accepted MessageAcceptedResponse
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil || accepted.ID != "msg_1" {
		t.Fatalf("response=%+v err=%v", accepted, err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.sentEmails) != 1 {
		t.Fatalf("sent=%d", len(mock.sentEmails))
	}
	email := mock.sentEmails[0].Email
	if email.From != `"Alice" <sender@example.com>` || email.To[0] != `"Bob" <to1@example.com>` {
		t.Fatalf("addresses: %+v", email)
	}
	if email.Headers["X-Tracking"] != "one" || email.Headers["X-Entity-ID"] != "two" {
		t.Fatalf("headers=%v", email.Headers)
	}
	if email.ReplyTo != `"Support" <reply@example.com>` {
		t.Fatalf("replyTo=%q", email.ReplyTo)
	}
	if got := string(email.Attachments[0].Content); got != "Hello attachment" {
		t.Fatalf("attachment=%q", got)
	}
	if mock.sentEmails[0].IdempotencyKey != "key-1" {
		t.Fatalf("key=%q", mock.sentEmails[0].IdempotencyKey)
	}
}

func TestIdempotency(t *testing.T) {
	a, mock := testApp()
	body := `{"from":{"email":"sender@example.com"},"to":[{"email":"to@example.com"}]}`
	first := request(t, a.routes(), http.MethodPost, "/v1/messages", "application/json", body, "same-key")
	second := request(t, a.routes(), http.MethodPost, "/v1/messages", "application/json", body, "same-key")
	conflict := request(t, a.routes(), http.MethodPost, "/v1/messages", "application/json", strings.Replace(body, "to@example.com", "other@example.com", 1), "same-key")
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() {
		t.Fatalf("first=%v second=%v", first, second)
	}
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
	mock.mu.Lock()
	sent := len(mock.sentEmails)
	mock.mu.Unlock()
	if sent != 1 {
		t.Fatalf("sent=%d", sent)
	}
}

func TestRequestErrors(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		status                  int
	}{
		{"missing content type", "", `{}`, 415},
		{"wrong content type", "text/plain", `{}`, 415},
		{"malformed json", "application/json", `{bad`, 400},
		{"unknown field", "application/json", `{"from":{"email":"a@example.com","extra":true},"to":[{"email":"b@example.com"}]}`, 400},
		{"invalid from", "application/json", `{"from":{"email":"bad"},"to":[{"email":"b@example.com"}]}`, 422},
		{"empty to", "application/json", `{"from":{"email":"a@example.com"},"to":[]}`, 422},
		{"bad attachment", "application/json", `{"from":{"email":"a@example.com"},"to":[{"email":"b@example.com"}],"attachments":[{"filename":"x","contentType":"text/plain","content":"!!!"}]}`, 422},
		{"duplicate header", "application/json", `{"from":{"email":"a@example.com"},"to":[{"email":"b@example.com"}],"headers":[{"name":"X-Test","value":"one"},{"name":"x-test","value":"two"}]}`, 422},
		{"header injection", "application/json", `{"from":{"email":"a@example.com","name":"bad\r\nBcc: x@example.com"},"to":[{"email":"b@example.com"}]}`, 422},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, _ := testApp()
			response := request(t, a.routes(), http.MethodPost, "/v1/messages", test.contentType, test.body, "")
			if response.Code != test.status {
				t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("content-type=%q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestCaseInsensitiveContentTypeAndBodyLimit(t *testing.T) {
	a, _ := testApp()
	body := `{"from":{"email":"a@example.com"},"to":[{"email":"b@example.com"}]}`
	if got := request(t, a.routes(), http.MethodPost, "/v1/messages", "Application/JSON; charset=utf-8", body, "").Code; got != 200 {
		t.Fatalf("code=%d", got)
	}
	a.maxBodySize = 32
	if got := request(t, a.routes(), http.MethodPost, "/v1/messages", "application/json", body, "").Code; got != 413 {
		t.Fatalf("code=%d", got)
	}
}
