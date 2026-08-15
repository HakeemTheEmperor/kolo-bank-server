// Command api runs the Kolo Bank backend HTTP server.
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

	"github.com/toluwalase/kolo-bank-server/internal/config"
	"github.com/toluwalase/kolo-bank-server/internal/httpserver"
	"github.com/toluwalase/kolo-bank-server/internal/observability"
	"github.com/toluwalase/kolo-bank-server/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal startup error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := observability.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	shutdownTracing, err := observability.InitTracing(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	shutdownMetrics, err := observability.InitMetrics(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownMetrics(context.Background()) }()

	db, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	handler := httpserver.New(logger, db)
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("starting server", slog.String("addr", cfg.HTTPAddr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
