package publicapi

import (
	"net/http"
	"strconv"
	"time"
)

const timeFormat = time.RFC3339

const (
	defaultListLimit = 25
	maxListLimit     = 100
)

func listLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultListLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}
