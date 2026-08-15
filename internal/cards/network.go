package cards

import "strings"

// declineMarker in a merchant name deterministically fails network
// simulation, the same controllable-simulation convention as
// internal/rails' RAILFAIL and internal/risk's FRAUDSCORE — a stand-in for
// a real card-network adapter (docs/banking-backend-spec.md §3.5: "against
// stubbed card-network adapters"), which is a non-goal for this build.
const declineMarker = "NETWORKDECLINE"

func simulateNetwork(merchantName string) (approved bool, declineReason string) {
	if strings.Contains(merchantName, declineMarker) {
		return false, "network_declined"
	}
	return true, ""
}
