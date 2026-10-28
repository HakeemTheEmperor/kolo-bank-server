//go:build stress && !stress_nolock

package stresstest

// lockMode labels results from the default build, where the ledger's
// hold row lock is in place.
const lockMode = "on"
