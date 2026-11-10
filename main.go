package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel})))

	provider, err := newProvider(cfg)
	if err != nil {
		slog.Error("initialize EdgeCDN-X API client", "error", err)
		os.Exit(1)
	}
	handler := newWebhookServer(provider)
	server := &http.Server{
		Addr:              cfg.listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shut down webhook server", "error", err)
		}
	}()

	slog.Info("starting ExternalDNS webhook", "address", cfg.listenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("webhook server failed", "error", err)
		os.Exit(1)
	}
}
