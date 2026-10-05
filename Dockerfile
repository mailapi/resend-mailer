FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN mkdir -p /data
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /resend-mailer .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /resend-mailer /resend-mailer

COPY --from=builder --chown=65532:65532 /data/ /data/
ENV MAILAPI_STATE_FILE=/data/submissions.json

ENTRYPOINT ["/resend-mailer"]
