# Resend Mailer

An API server that implements the message-sending endpoint from the [Mail API Specification](https://github.com/mailapi/mailapi/blob/main/openapi.yaml) using the [official Resend Go SDK](https://github.com/resend/resend-go).

## Features

- `POST /v1/messages`: converts Mail API requests and sends them through Resend
- `GET /health` and `GET /v1/health`: health checks
- RFC 9457 Problem Details error responses
- 24-hour `Idempotency-Key` cache based on the SHA-256 hash of the raw request body
- CC, BCC, Reply-To, custom headers, and Base64 attachments
- 10 MiB request limit and graceful shutdown

## Requirements

- Go 1.26

## Run

| Environment variable | Description | Default |
| --- | --- | --- |
| `RESEND_API_KEY` | Resend API key (`re_...`) | Required unless mock mode is enabled |
| `MOCK_MAILER` | Logs simulated delivery without sending email | `false` |
| `ALLOW_MOCK_MAILER` | Compatibility alias for `MOCK_MAILER` | `false` |
| `PORT` | HTTP listening port | `8080` |

```bash
# Send through Resend.
RESEND_API_KEY=re_123456789 go run .

# Local simulated delivery.
MOCK_MAILER=true go run .

go test -race ./...
go vet ./...
```

## Example request

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: welcome-user/123456' \
  -d '{
    "from": {"email": "onboarding@resend.dev", "name": "Acme Team"},
    "to": [{"email": "user@example.com", "name": "John Doe"}],
    "subject": "Welcome!",
    "text": "Thank you for joining us.",
    "html": "<h1>Welcome!</h1>",
    "replyTo": [{"email": "support@example.com"}],
    "headers": [{"name": "X-Campaign-ID", "value": "2026-q1"}],
    "attachments": [{"filename": "hello.txt", "contentType": "text/plain", "content": "SGVsbG8="}]
  }'
```

A successful response has the form `{"id":"e30e66bd-8949-41e7-9154-b67f4077ff0a"}`.

## Docker and Kubernetes

```bash
docker build -t resend-mailer:latest .
docker run --rm -p 8080:8080 -e RESEND_API_KEY=re_123456789 resend-mailer:latest

kubectl apply -f deploy/namespace.yaml
kubectl -n mailapi create secret generic resend-mailer-secret \
  --from-literal=RESEND_API_KEY=re_123456789 \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/
```

See [deploy/README.md](deploy/README.md) for deployment setup and operational limitations.
