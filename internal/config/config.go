// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	// HTTPAddr is the address the API server listens on, e.g. ":8080".
	HTTPAddr string
	// DatabaseURL is the Postgres connection string (DSN).
	DatabaseURL string
	// LogLevel is one of: debug, info, warn, error.
	LogLevel string
	// OTLPEndpoint is the OpenTelemetry collector endpoint. Empty disables export.
	OTLPEndpoint string
	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight requests.
	ShutdownTimeout time.Duration
	// PublicBaseURL is used to build fully-qualified URLs returned to API
	// clients, e.g. checkout.Session.RedirectURL.
	PublicBaseURL string
}

// Load reads configuration from the environment. It fails fast on missing
// required values so misconfiguration is caught at startup, not at first use.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL:     getEnv("DATABASE_URL", ""),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		OTLPEndpoint:    getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ShutdownTimeout: 10 * time.Second,
		PublicBaseURL:   getEnv("PUBLIC_BASE_URL", "http://localhost:8080"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
