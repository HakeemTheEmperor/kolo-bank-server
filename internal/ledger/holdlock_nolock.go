//go:build stress_nolock

package ledger

// holdRowLock is empty only in internal/stresstest's comparison builds
// (-tags stress_nolock). See holdlock.go.
const holdRowLock = ""
