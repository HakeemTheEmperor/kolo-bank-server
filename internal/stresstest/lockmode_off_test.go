//go:build stress && stress_nolock

package stresstest

// lockMode labels results from the -tags stress_nolock build, where the
// ledger's hold row lock is removed (internal/ledger/holdlock_nolock.go).
const lockMode = "off"
