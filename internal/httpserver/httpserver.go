// Package httpserver builds the HTTP server: routing, health/readiness
// endpoints, and cross-cutting request middleware (logging, tracing).
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/toluwalase/kolo-bank-server/docs"
)

const tracerName = "kolo-bank-server/httpserver"

// Pinger is satisfied by anything that can check liveness of a dependency,
// e.g. a database connection pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New builds the top-level HTTP handler. db is used for the readiness check;
// it may be nil if no dependency needs checking yet. apiHandler (may be
// nil) is mounted at /v1/ — the public integration API
// (internal/publicapi) — so every route shares this same logging/tracing
// middleware rather than each package building its own.
func New(logger *slog.Logger, db Pinger, apiHandler http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(db))
	mux.HandleFunc("GET /docs", handleDocs)
	if apiHandler != nil {
		mux.Handle("/v1/", apiHandler)
	}

	return withMiddleware(mux, logger)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleDocs serves the API reference (docs/api-reference.html) as-is —
// a static, self-contained page, embedded into the binary at build time so
// it needs no separate deploy step or file-serving setup.
func handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(docs.APIReferenceHTML)
}

func handleReadyz(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := db.Ping(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("not ready: " + err.Error()))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}

func withMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	tracer := otel.Tracer(tracerName)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		defer span.End()

		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		logger.InfoContext(ctx, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
