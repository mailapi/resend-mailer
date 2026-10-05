package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	rateLimit := 2
	if raw := os.Getenv("RESEND_RATE_LIMIT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return errors.New("RESEND_RATE_LIMIT must be between 1 and 100 requests per second")
		}
		rateLimit = parsed
	}
	var mailer mailerClient
	if apiKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); apiKey != "" {
		slog.Info("Initializing Resend client with RESEND_API_KEY", "rate_limit", rateLimit)
		mailer = newResendMailerClient(apiKey, rateLimit)
	} else if envTrue("MOCK_MAILER") || envTrue("ALLOW_MOCK_MAILER") {
		slog.Warn("MOCK_MAILER is enabled; emails will not be sent")
		mailer = &mockMailerClient{}
	} else {
		return errors.New("RESEND_API_KEY is not set; set MOCK_MAILER=true for local mock mode")
	}

	token := strings.TrimSpace(os.Getenv("MAILAPI_TOKEN"))
	if token == "" || !visibleASCII(token) {
		return errors.New("MAILAPI_TOKEN must be set to a non-empty visible ASCII bearer token")
	}
	stateFile := os.Getenv("MAILAPI_STATE_FILE")
	if stateFile == "" {
		stateFile = "data/submissions.json"
	}
	store, err := openIdempotencyStore(stateFile)
	if err != nil {
		return fmt.Errorf("open submission journal: %w", err)
	}
	defer store.close()
	application := newApp(mailer)
	application.token = token
	application.principal = strings.TrimSpace(os.Getenv("MAILAPI_PRINCIPAL"))
	limit := 2
	if raw := os.Getenv("MAILAPI_CONCURRENCY_LIMIT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 32 {
			return errors.New("MAILAPI_CONCURRENCY_LIMIT must be between 1 and 32")
		}
		limit = parsed
	}
	application.slots = make(chan struct{}, limit)
	application.idempotency = store
	application.allowedFrom = make(map[string]bool)
	for _, address := range strings.Split(os.Getenv("MAILAPI_ALLOWED_FROM"), ",") {
		if address = strings.TrimSpace(address); address != "" {
			application.allowedFrom[strings.ToLower(address)] = true
		}
	}
	cleanupDone := make(chan struct{})
	cleanupStopped := make(chan struct{})
	go func() {
		defer close(cleanupStopped)
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				application.idempotency.cleanup()
			case <-cleanupDone:
				return
			}
		}
	}()
	defer func() { close(cleanupDone); <-cleanupStopped }()
	defer application.workers.Wait()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: application.routes(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()

	slog.Info("Starting Mail API server", "address", "http://0.0.0.0:"+port)
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
	case <-ctx.Done():
		slog.Info("Shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server stopped unexpectedly: %w", err)
		}
	}
	slog.Info("Server shut down successfully")
	return nil
}

func envTrue(name string) bool {
	value := os.Getenv(name)
	return value == "1" || strings.EqualFold(value, "true")
}
