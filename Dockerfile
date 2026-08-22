FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /resend-mailer .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /resend-mailer /resend-mailer

ENV PORT=3000
EXPOSE 3000
ENTRYPOINT ["/resend-mailer"]
