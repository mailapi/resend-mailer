package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	resend "github.com/resend/resend-go/v3"
)

type mailerClient interface {
	Send(context.Context, *resend.SendEmailRequest, string) (*resend.SendEmailResponse, *appError)
}

type resendMailerClient struct{ client *resend.Client }

type providerResponseMetadata struct {
	status     int
	retryAfter string
	name       string
	message    string
}

type providerResponseMetadataContextKey struct{}

type responseCapturingTransport struct{ next http.RoundTripper }

func (t responseCapturingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil || response == nil || response.StatusCode < 300 {
		return response, err
	}

	metadata, ok := request.Context().Value(providerResponseMetadataContextKey{}).(*providerResponseMetadata)
	if !ok {
		return response, nil
	}
	metadata.status = response.StatusCode
	metadata.retryAfter = response.Header.Get("Retry-After")
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return response, nil
	}
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	var providerError struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &providerError) == nil {
		metadata.name = providerError.Name
		metadata.message = providerError.Message
	}
	return response, nil
}

func newResendMailerClient(apiKey string) *resendMailerClient {
	httpClient := &http.Client{
		Timeout:   time.Minute,
		Transport: responseCapturingTransport{next: http.DefaultTransport},
	}
	return &resendMailerClient{client: resend.NewCustomClient(httpClient, apiKey)}
}

func (c *resendMailerClient) Send(ctx context.Context, email *resend.SendEmailRequest, key string) (*resend.SendEmailResponse, *appError) {
	metadata := &providerResponseMetadata{}
	ctx = context.WithValue(ctx, providerResponseMetadataContextKey{}, metadata)
	response, err := c.client.Emails.SendWithOptions(ctx, email, &resend.SendEmailOptions{IdempotencyKey: key})
	if err == nil {
		return response, nil
	}
	slog.Error("Resend API error", "error", err)
	if metadata.status != 0 {
		return nil, mapProviderError(metadata, err)
	}
	var rateLimit *resend.RateLimitError
	if errors.As(err, &rateLimit) {
		return nil, &appError{status: http.StatusTooManyRequests, retryAfter: rateLimit.RetryAfter, problem: Problem{
			Type: "https://api.example.com/problems/rate-limit-exceeded", Title: "Submission rate limit exceeded", Status: http.StatusTooManyRequests, Detail: rateLimit.Message,
		}}
	}
	return nil, newAppError(http.StatusInternalServerError, "internal-server-error", "Unexpected provider error", strings.TrimPrefix(err.Error(), "[ERROR]: "))
}

func mapProviderError(metadata *providerResponseMetadata, cause error) *appError {
	detail := metadata.message
	if detail == "" {
		detail = strings.TrimPrefix(cause.Error(), "[ERROR]: ")
	}
	switch metadata.status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return unprocessable(detail)
	case http.StatusConflict:
		switch metadata.name {
		case "invalid_idempotent_request":
			return idempotencyReused()
		case "concurrent_idempotent_requests":
			return idempotencyInProgress()
		default:
			return newAppError(http.StatusConflict, "provider-conflict", "Provider conflict", detail)
		}
	case http.StatusTooManyRequests:
		return &appError{status: http.StatusTooManyRequests, retryAfter: metadata.retryAfter, problem: Problem{
			Type: "https://api.example.com/problems/rate-limit-exceeded", Title: "Submission rate limit exceeded", Status: http.StatusTooManyRequests, Detail: detail,
		}}
	case http.StatusServiceUnavailable:
		return &appError{status: http.StatusServiceUnavailable, retryAfter: metadata.retryAfter, problem: Problem{
			Type: "https://api.example.com/problems/service-unavailable", Title: "Service Unavailable", Status: http.StatusServiceUnavailable, Detail: detail,
		}}
	default:
		return newAppError(http.StatusInternalServerError, "internal-server-error", "Unexpected provider error", detail)
	}
}

type sentEmailRecord struct {
	Email          *resend.SendEmailRequest
	IdempotencyKey string
}

type mockMailerClient struct {
	mu         sync.Mutex
	sentEmails []sentEmailRecord
}

func (c *mockMailerClient) Send(_ context.Context, email *resend.SendEmailRequest, key string) (*resend.SendEmailResponse, *appError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sentEmails = append(c.sentEmails, sentEmailRecord{Email: email, IdempotencyKey: key})
	id := fmt.Sprintf("msg_%d", len(c.sentEmails))
	slog.Info("Dispatched simulated email", "mock_id", id, "idempotency_key", key)
	return &resend.SendEmailResponse{Id: id}, nil
}

func buildResendEmail(req *OutboundMessageRequest) (*resend.SendEmailRequest, *appError) {
	if err := validateAddress(req.From); err != nil {
		return nil, unprocessable(err.Error())
	}
	if len(req.To) == 0 {
		return nil, unprocessable("'to' field must contain at least 1 recipient email address")
	}
	for label, addresses := range map[string][]EmailAddress{"to": req.To, "cc": req.Cc, "bcc": req.Bcc, "replyTo": req.ReplyTo} {
		for i, address := range addresses {
			if err := validateAddress(address); err != nil {
				return nil, unprocessable(fmt.Sprintf("'%s' address at index %d: %v", label, i, err))
			}
		}
	}

	email := &resend.SendEmailRequest{
		From: formatAddress(req.From), To: formatAddresses(req.To), Cc: formatAddresses(req.Cc), Bcc: formatAddresses(req.Bcc), Headers: make(map[string]string),
	}
	if req.Subject != nil {
		email.Subject = *req.Subject
	}
	if req.Text != nil {
		email.Text = *req.Text
	}
	if req.HTML != nil {
		email.Html = *req.HTML
	}
	// resend-go models ReplyTo as one string; an RFC 5322 mailbox list preserves all Mail API recipients.
	email.ReplyTo = strings.Join(formatAddresses(req.ReplyTo), ", ")
	seenHeaders := make(map[string]struct{}, len(req.Headers))
	for _, header := range req.Headers {
		if strings.TrimSpace(header.Name) == "" {
			return nil, unprocessable("Header name cannot be empty")
		}
		if strings.ContainsAny(header.Name, "\r\n") || strings.ContainsAny(header.Value, "\r\n") {
			return nil, unprocessable(fmt.Sprintf("Header '%s' contains invalid newline characters", header.Name))
		}
		canonicalName := strings.ToLower(header.Name)
		if _, exists := seenHeaders[canonicalName]; exists {
			return nil, unprocessable(fmt.Sprintf("Duplicate header '%s' is not supported by the Resend provider", header.Name))
		}
		seenHeaders[canonicalName] = struct{}{}
		email.Headers[header.Name] = header.Value
	}
	if len(email.Headers) == 0 {
		email.Headers = nil
	}
	for _, attachment := range req.Attachments {
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, attachment.Content)
		content, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, unprocessable(fmt.Sprintf("Failed to decode base64 content for attachment '%s': %v", attachment.Filename, err))
		}
		email.Attachments = append(email.Attachments, &resend.Attachment{Filename: attachment.Filename, ContentType: attachment.ContentType, Content: content})
	}
	return email, nil
}

func validateAddress(address EmailAddress) error {
	if strings.ContainsAny(address.Email, "\r\n") {
		return fmt.Errorf("invalid email address syntax: '%s'", address.Email)
	}
	parsed, err := mail.ParseAddress(address.Email)
	if err != nil || parsed.Address != address.Email {
		return fmt.Errorf("invalid email address syntax: '%s'", address.Email)
	}
	if address.Name != nil && strings.ContainsAny(*address.Name, "\r\n") {
		return fmt.Errorf("display name contains prohibited newline characters: '%s'", *address.Name)
	}
	return nil
}

func formatAddress(address EmailAddress) string {
	if address.Name == nil || strings.TrimSpace(*address.Name) == "" {
		return address.Email
	}
	return (&mail.Address{Name: strings.TrimSpace(*address.Name), Address: address.Email}).String()
}

func formatAddresses(addresses []EmailAddress) []string {
	result := make([]string, len(addresses))
	for i, address := range addresses {
		result[i] = formatAddress(address)
	}
	return result
}
