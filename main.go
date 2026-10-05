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
	rateLimit, err := envInt("RESEND_RATE_LIMIT", 2, 1, 100)
	if err != nil {
		return err
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
	limit, err := envInt("MAILAPI_CONCURRENCY_LIMIT", 2, 1, 32)
	if err != nil {
		return err
	}
	application.slots = make(chan struct{}, limit)
	queueLimit, err := envInt("MAILAPI_QUEUE_LIMIT", defaultQueueLimit, 1, 1000)
	if err != nil {
		return err
	}
	queueMaxBytes, err := envInt("MAILAPI_QUEUE_MAX_BYTES", defaultQueueMaxBytes, 1, 1<<30)
	if err != nil {
		return err
	}
	application.queue = newAdmissionQueue(queueLimit, int64(queueMaxBytes))
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

func envInt(name string, fallback, min, max int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}
