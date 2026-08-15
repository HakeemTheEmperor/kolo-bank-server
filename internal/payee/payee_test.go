package payee_test

import (
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/payee"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		typed  string
		actual string
		want   payee.Result
	}{
		{"exact match", "Jane Doe", "Jane Doe", payee.Match},
		{"case and spacing insensitive", "  jane   doe ", "Jane Doe", payee.Match},
		{"typo is close match", "Jane Dooe", "Jane Doe", payee.CloseMatch},
		{"partial name is close match", "Jane", "Jane Doe", payee.CloseMatch},
		{"different person is no match", "John Smith", "Jane Doe", payee.NoMatch},
		{"empty typed name is no match", "", "Jane Doe", payee.NoMatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := payee.Check(tt.typed, tt.actual)
			if got != tt.want {
				t.Fatalf("Check(%q, %q) = %s, want %s", tt.typed, tt.actual, got, tt.want)
			}
		})
	}
}
