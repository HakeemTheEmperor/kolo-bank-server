//go:build !stress_nolock

package ledger

// holdRowLock is appended to the SELECT that reads a hold's status before
// capturing or releasing it. The row lock is what stops a concurrent
// capture and release of the same hold from both seeing it "active".
// internal/stresstest builds with -tags stress_nolock (holdlock_nolock.go)
// to measure what happens without it; no other build uses that tag.
const holdRowLock = " FOR UPDATE"
