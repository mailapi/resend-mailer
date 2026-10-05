# Resend Mailer

An API server that implements the message-sending endpoint from the [Mail API Specification](https://github.com/mailapi/mailapi/blob/v0.4.4/openapi.yaml) using the [official Resend Go SDK](https://github.com/resend/resend-go).

## Features

- `POST /v1/messages`: converts Mail API requests and sends them through Resend
- `GET /health` and `GET /v1/health`: liveness; `GET /ready`: journal write readiness
- RFC 9457 Problem Details error responses
- Bearer authentication and optional sender authorization
- Default `202` acceptance; bounded `Prefer: wait=N` support (`0`–`60` seconds)
- 24-hour principal-scoped idempotency journal, including terminal `200`/`500` replay
- CC, BCC, Reply-To, custom headers, and Base64 attachments
- 10 MiB request limit and graceful shutdown

## Requirements

- Go 1.26

## Run

| Environment variable | Description | Default |
| --- | --- | --- |
| `MAILAPI_TOKEN` | Provider-issued bearer token accepted from clients | Required, including mock mode |
| `MAILAPI_PRINCIPAL` | Stable idempotency namespace across token rotation; use a non-secret identifier for new installations; legacy namespace adoption is described below | Defaults to `MAILAPI_TOKEN` |
| `MAILAPI_CONCURRENCY_LIMIT` | Concurrent submissions, from 1 to 32 | `2` |
| `MAILAPI_QUEUE_LIMIT` | Accepted submissions waiting or sending, from 1 to 1000; at the default rate, 20 drain in about ten seconds | `20` |
| `MAILAPI_QUEUE_MAX_BYTES` | Total request bytes of those submissions; a single request is always admitted into an empty queue | `33554432` (32 MiB) |
| `RESEND_RATE_LIMIT` | Resend requests per second, from 1 to 100; match your Resend account limit | `2` |
| `MAILAPI_ALLOWED_FROM` | Comma-separated sender addresses this token may use; empty permits all | `""` |
| `MAILAPI_STATE_FILE` | Persistent submission journal; one process per file | `data/submissions.json` (`/data/submissions.json` in Docker) |
| `RESEND_API_KEY` | Resend API key (`re_...`) | Required unless mock mode is enabled |
| `MOCK_MAILER` | Logs simulated delivery without sending email | `false` |
| `ALLOW_MOCK_MAILER` | Compatibility alias for `MOCK_MAILER` | `false` |
| `PORT` | HTTP listening port | `8080` |

```bash
# Send through Resend.
MAILAPI_TOKEN=local-test-token RESEND_API_KEY=re_123456789 go run .

# Local simulated delivery.
MAILAPI_TOKEN=local-test-token MOCK_MAILER=true go run .

go test -race ./...
go vet ./...
```

## Example request

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H 'Authorization: Bearer local-test-token' \
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

A successful response has the form `{"id":"msg_..."}`. The Mail API ID is assigned before dispatch and stays unchanged between acceptance and terminal replay; the downstream Resend ID is logged separately. Acceptance is not a delivery receipt.

Without `Prefer`, submission returns `202` and continues independently of the client connection. Add `Prefer: wait=10` to wait for up to ten seconds: a completed submission returns `200` (or a terminal `500`), otherwise it returns `202`. Applied waits include `Preference-Applied`. Invalid or unsupported preferences are ignored.

Matching keyed retries during execution return `409`; after completion they replay the exact terminal status/body with `Idempotency-Replayed: true`. Input validation, authorization, and queue admission happen before reservation. Accepted submissions wait in a bounded queue for one of `MAILAPI_CONCURRENCY_LIMIT` dispatch slots, so a client sending sequentially without `Prefer` is queued rather than rejected while earlier messages are still sending. A full queue returns `429` with `Retry-After: 1`; an unavailable journal returns `503` before execution. Resend requests are paced at `RESEND_RATE_LIMIT` per second (two by default). Confirmed downstream 429/503 rejections are retried at most twice with the same provider key, honoring Retry-After of at most two seconds and leaving at least two seconds for another request within the one-minute dispatch deadline; ambiguous transport failures are not retried. Failures after downstream dispatch begins become terminal `500` outcomes and are retained so retries cannot dispatch again.

At least one recipient across `to`, `cc`, and `bcc`, and at least one of `text`/`html`, are required. Explicit empty `to`/`cc`/`bcc` lists, null fields, invalid header names, and structured/MIME framing headers are rejected. Unknown members inside `extensions` are ignored. Resend represents custom headers as a map, so repeated case-insensitive header names are a documented provider limitation and produce `422`.

## Upgrading from v0.2.x

Set `MAILAPI_TOKEN` on the server and configure the same bearer token in clients (MediaWiki: `$wgMailAPIToken`). Clients must accept `202` as well as `200`. Mount a durable journal volume and run one replica; when `MAILAPI_PRINCIPAL` is unset, changing the token establishes a different idempotency namespace. Set `MAILAPI_PRINCIPAL` to a stable identifier for future token rotation; for new installations, choose a non-secret identifier such as `wiki-production` before accepting submissions. For an existing journal using the token fallback, retaining its namespace requires the previous token value; inject that value only from a Secret, never a ConfigMap or public configuration. Keep the volume across restarts. Interrupted keyed executions recover as terminal `500` outcomes, never as new submissions. Authentication and the response contract are breaking changes.

## Docker and Kubernetes

```bash
docker build -t resend-mailer:latest .
docker run --rm -p 8080:8080 -v resend-mailer-data:/data \
  -e MAILAPI_TOKEN=local-test-token -e RESEND_API_KEY=re_123456789 resend-mailer:latest

kubectl apply -f deploy/namespace.yaml
kubectl -n mailapi create secret generic resend-mailer-secret \
  --from-literal=RESEND_API_KEY=re_123456789 \
  --from-literal=MAILAPI_TOKEN=your-private-bearer-token \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k deploy/
```

See [deploy/README.md](deploy/README.md) for deployment setup and operational limitations.

If terminal journal persistence fails, the running process preserves the actual result for replay and logs the failure. After a restart, the pending disk entry recovers as an unknown-outcome terminal 500. Readiness checks test journal-directory writes; liveness remains available.

Unkeyed API submissions receive a generated provider idempotency key that is retained across internal retries. Separate unkeyed API calls remain separate submissions. In-progress keyed requests return `Retry-After: 1`. Readiness failures remove the Pod from Service routing, so clients may receive connection errors rather than an HTTP 503.
