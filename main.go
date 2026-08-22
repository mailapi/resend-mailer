package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	var mailer mailerClient
	if apiKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); apiKey != "" {
		slog.Info("Initializing Resend client with RESEND_API_KEY")
		mailer = newResendMailerClient(apiKey)
	} else if envTrue("MOCK_MAILER") || envTrue("ALLOW_MOCK_MAILER") {
		slog.Warn("MOCK_MAILER is enabled; emails will not be sent")
		mailer = &mockMailerClient{}
	} else {
		return errors.New("RESEND_API_KEY is not set; set MOCK_MAILER=true for local mock mode")
	}

	application := newApp(mailer)
	cleanupDone := make(chan struct{})
	go func() {
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
	defer close(cleanupDone)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
