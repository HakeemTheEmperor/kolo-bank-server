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

	"github.com/toluwalase/kolo-bank-server/internal/apikeys"
	"github.com/toluwalase/kolo-bank-server/internal/auth"
	"github.com/toluwalase/kolo-bank-server/internal/bills"
	"github.com/toluwalase/kolo-bank-server/internal/charges"
	"github.com/toluwalase/kolo-bank-server/internal/checkout"
	"github.com/toluwalase/kolo-bank-server/internal/compliance"
	"github.com/toluwalase/kolo-bank-server/internal/config"
	"github.com/toluwalase/kolo-bank-server/internal/externalpayments"
	"github.com/toluwalase/kolo-bank-server/internal/fees"
	"github.com/toluwalase/kolo-bank-server/internal/httpserver"
	"github.com/toluwalase/kolo-bank-server/internal/identity"
	"github.com/toluwalase/kolo-bank-server/internal/ledger"
	"github.com/toluwalase/kolo-bank-server/internal/observability"
	"github.com/toluwalase/kolo-bank-server/internal/payments"
	"github.com/toluwalase/kolo-bank-server/internal/payouts"
	"github.com/toluwalase/kolo-bank-server/internal/postgres"
	"github.com/toluwalase/kolo-bank-server/internal/publicapi"
	"github.com/toluwalase/kolo-bank-server/internal/rails"
	"github.com/toluwalase/kolo-bank-server/internal/reconciliation"
	"github.com/toluwalase/kolo-bank-server/internal/risk"
	"github.com/toluwalase/kolo-bank-server/internal/scheduler"
	"github.com/toluwalase/kolo-bank-server/internal/secrets"
	"github.com/toluwalase/kolo-bank-server/internal/settlement"
	"github.com/toluwalase/kolo-bank-server/internal/tokens"
	"github.com/toluwalase/kolo-bank-server/internal/webhooks"
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

	riskSvc := risk.NewService(db.Pool, ledgerSvc, compliance.NewStubScreener(), logger)

	railRegistry := rails.NewRegistry()
	externalSvc := externalpayments.NewService(db.Pool, ledgerSvc, railRegistry, riskSvc, logger)
	billsSvc := bills.NewService(db.Pool, externalSvc)

	keyProvider := secrets.NewLocalKeyProvider()
	authSvc := auth.NewService(db.Pool, identitySvc, keyProvider)
	apiKeysSvc := apikeys.NewService(db.Pool)
	tokensSvc := tokens.NewService(db.Pool)
	chargesSvc := charges.NewService(db.Pool, tokensSvc, externalSvc)
	payoutsSvc := payouts.NewService(db.Pool, externalSvc, railRegistry)
	checkoutSvc := checkout.NewService(db.Pool)
	webhooksSvc := webhooks.NewService(db.Pool, keyProvider)

	feesSvc := fees.NewService(db.Pool, ledgerSvc, logger)
	settlementSvc := settlement.NewService(db.Pool, payoutsSvc, logger)
	reconciliationSvc := reconciliation.NewService(db.Pool, logger)

	go runScheduler(ctx, schedulerSvc, billsSvc, logger)
	go runExternalPayments(ctx, externalSvc, logger)
	go runWebhooks(ctx, webhooksSvc, logger)
	go runMoneyControls(ctx, feesSvc, settlementSvc, reconciliationSvc, logger)
	go runComplianceReports(ctx, riskSvc, logger)

	apiHandler := publicapi.New(publicapi.Deps{
		ApiKeys:       apiKeysSvc,
		Auth:          authSvc,
		Identity:      identitySvc,
		Tokens:        tokensSvc,
		Charges:       chargesSvc,
		Payouts:       payoutsSvc,
		Checkout:      checkoutSvc,
		Webhooks:      webhooksSvc,
		Logger:        logger,
		PublicBaseURL: cfg.PublicBaseURL,
	})
	handler := httpserver.New(logger, db, apiHandler)
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

// runScheduler periodically executes due scheduled transfers and recurring
// bill payments, and sweeps scheduled transfers left stuck mid-execution
// (docs/banking-backend-spec.md §3.4, §3.7, §4.4). It stops when ctx is
// cancelled, e.g. on shutdown signal.
func runScheduler(ctx context.Context, schedulerSvc *scheduler.Service, billsSvc *bills.Service, logger *slog.Logger) {
	dueTicker := time.NewTicker(10 * time.Second)
	defer dueTicker.Stop()
	stuckTicker := time.NewTicker(60 * time.Second)
	defer stuckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dueTicker.C:
			if err := schedulerSvc.RunDue(ctx); err != nil {
				logger.ErrorContext(ctx, "scheduler: run due failed", slog.Any("error", err))
			}
			if err := billsSvc.RunDueBills(ctx); err != nil {
				logger.ErrorContext(ctx, "bills: run due failed", slog.Any("error", err))
			}
		case <-stuckTicker.C:
			if err := schedulerSvc.ResolveStuck(ctx); err != nil {
				logger.ErrorContext(ctx, "scheduler: resolve stuck failed", slog.Any("error", err))
			}
		}
	}
}

// runExternalPayments periodically drives pending external-rail transfers
// (bill payments included, since they route through the same rail) and
// sweeps any left stuck mid-flight — the in-flight resolver
// (docs/banking-backend-spec.md §4.4). It stops when ctx is cancelled.
func runExternalPayments(ctx context.Context, svc *externalpayments.Service, logger *slog.Logger) {
	pendingTicker := time.NewTicker(3 * time.Second)
	defer pendingTicker.Stop()
	stuckTicker := time.NewTicker(60 * time.Second)
	defer stuckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pendingTicker.C:
			if err := svc.ProcessPending(ctx); err != nil {
				logger.ErrorContext(ctx, "externalpayments: process pending failed", slog.Any("error", err))
			}
		case <-stuckTicker.C:
			if err := svc.ResolveStuck(ctx); err != nil {
				logger.ErrorContext(ctx, "externalpayments: resolve stuck failed", slog.Any("error", err))
			}
		}
	}
}

// runWebhooks periodically enqueues webhook events for newly-terminal
// charges/payouts and delivers due (including backed-off retry) deliveries
// (docs/banking-backend-spec.md §3.6). It stops when ctx is cancelled.
func runWebhooks(ctx context.Context, svc *webhooks.Service, logger *slog.Logger) {
	notifyTicker := time.NewTicker(3 * time.Second)
	defer notifyTicker.Stop()
	deliverTicker := time.NewTicker(5 * time.Second)
	defer deliverTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-notifyTicker.C:
			if err := svc.NotifyTerminal(ctx); err != nil {
				logger.ErrorContext(ctx, "webhooks: notify terminal failed", slog.Any("error", err))
			}
		case <-deliverTicker.C:
			if err := svc.DeliverPending(ctx); err != nil {
				logger.ErrorContext(ctx, "webhooks: deliver pending failed", slog.Any("error", err))
			}
		}
	}
}

// runMoneyControls periodically applies fees to newly-completed
// charges/payouts, runs due settlement cycles and matured reserve
// releases, and drives reconciliation — generating simulated statement
// lines and matching them against the ledger, routing genuine
// discrepancies to the break queue (docs/banking-backend-spec.md §4.1-§4.3).
// It stops when ctx is cancelled.
func runMoneyControls(ctx context.Context, feesSvc *fees.Service, settlementSvc *settlement.Service, reconciliationSvc *reconciliation.Service, logger *slog.Logger) {
	feesTicker := time.NewTicker(5 * time.Second)
	defer feesTicker.Stop()
	reconciliationTicker := time.NewTicker(15 * time.Second)
	defer reconciliationTicker.Stop()
	settlementTicker := time.NewTicker(30 * time.Second)
	defer settlementTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-feesTicker.C:
			if err := feesSvc.ApplyFees(ctx); err != nil {
				logger.ErrorContext(ctx, "fees: apply failed", slog.Any("error", err))
			}
		case <-reconciliationTicker.C:
			if err := reconciliationSvc.GenerateStatementLines(ctx); err != nil {
				logger.ErrorContext(ctx, "reconciliation: generate statement lines failed", slog.Any("error", err))
			}
			if err := reconciliationSvc.RunReconciliation(ctx); err != nil {
				logger.ErrorContext(ctx, "reconciliation: run failed", slog.Any("error", err))
			}
		case <-settlementTicker.C:
			if err := settlementSvc.RunCycles(ctx); err != nil {
				logger.ErrorContext(ctx, "settlement: run cycles failed", slog.Any("error", err))
			}
			if err := settlementSvc.ReleaseMaturedReserves(ctx); err != nil {
				logger.ErrorContext(ctx, "settlement: release matured reserves failed", slog.Any("error", err))
			}
		}
	}
}

// runComplianceReports periodically generates SAR/CTR-equivalent
// regulatory export jobs for a trailing day window (§3.8: "AML transaction
// reporting ... and regulatory reporting exports"). The interval is
// demo-scale, matching the other money-controls tickers above — the exit
// criterion is that reports generate, not real reporting-period semantics.
func runComplianceReports(ctx context.Context, riskSvc *risk.Service, logger *slog.Logger) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			periodEnd := time.Now()
			periodStart := periodEnd.Add(-24 * time.Hour)
			if _, err := riskSvc.GenerateSARReport(ctx, periodStart, periodEnd); err != nil {
				logger.ErrorContext(ctx, "risk: generate SAR report failed", slog.Any("error", err))
			}
			if _, err := riskSvc.GenerateCTRReport(ctx, periodStart, periodEnd); err != nil {
				logger.ErrorContext(ctx, "risk: generate CTR report failed", slog.Any("error", err))
			}
		}
	}
}
