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
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/observability"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/postgres"
	"github.com/toluwalase/kolo-bank-server/internal/scheduler"
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

	identitySvc := identity.NewService(db.Pool)
	ledgerSvc := ledger.NewService(db.Pool)
	paymentsSvc := payments.NewService(db.Pool, ledgerSvc, identitySvc)
	schedulerSvc := scheduler.NewService(db.Pool, paymentsSvc, logger)
	go runScheduler(ctx, schedulerSvc, logger)

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

// runScheduler periodically executes due scheduled transfers and sweeps
// any left stuck mid-execution (docs/banking-backend-spec.md §3.4, §4.4).
// It stops when ctx is cancelled, e.g. on shutdown signal.
func runScheduler(ctx context.Context, svc *scheduler.Service, logger *slog.Logger) {
	dueTicker := time.NewTicker(10 * time.Second)
	defer dueTicker.Stop()
	stuckTicker := time.NewTicker(60 * time.Second)
	defer stuckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dueTicker.C:
			if err := svc.RunDue(ctx); err != nil {
				logger.ErrorContext(ctx, "scheduler: run due failed", slog.Any("error", err))
			}
		case <-stuckTicker.C:
			if err := svc.ResolveStuck(ctx); err != nil {
				logger.ErrorContext(ctx, "scheduler: resolve stuck failed", slog.Any("error", err))
			}
		}
	}
}
