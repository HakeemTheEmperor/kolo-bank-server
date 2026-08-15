// Package payee implements confirmation of payee
// (docs/banking-backend-spec.md §5.1): before a P2P transfer confirms, the
// recipient name the sender typed is checked against the name on the
// destination account. Stateless and pure — no persistence, no external
// provider, unlike internal/compliance's or internal/kyc's stubs, because
// nothing about this check needs simulating; it's a straightforward string
// comparison a real bank would also just run locally.
package payee

import "strings"

type Result string

const (
	Match      Result = "match"
	CloseMatch Result = "close_match"
	NoMatch    Result = "no_match"
)

// Check compares typedName (what the sender entered) against actualName
// (the name on record for the resolved recipient account) and classifies
// the result as the sender would see it before confirming a transfer.
func Check(typedName, actualName string) Result {
	typed := normalize(typedName)
	actual := normalize(actualName)

	if typed == actual {
		return Match
	}
	if typed == "" || actual == "" {
		return NoMatch
	}
	if strings.Contains(actual, typed) || strings.Contains(typed, actual) {
		return CloseMatch
	}
	if levenshtein(typed, actual) <= closeMatchDistance {
		return CloseMatch
	}
	return NoMatch
}

// closeMatchDistance tolerates small typos (a transposed or missing
// character) without treating them as a full mismatch.
const closeMatchDistance = 2

func normalize(name string) string {
	fields := strings.Fields(strings.ToLower(name))
	return strings.Join(fields, " ")
}

// levenshtein computes the edit distance between a and b.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
