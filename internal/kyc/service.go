package kyc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RecordChecks writes one kyc_checks row per outcome within tx.
func RecordChecks(ctx context.Context, tx pgx.Tx, identityID string, outcomes []CheckOutcome) error {
	for _, o := range outcomes {
		rawJSON, err := json.Marshal(o.RawResult)
		if err != nil {
			return fmt.Errorf("kyc: marshal raw result: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO kyc_checks (identity_id, check_type, provider, result, raw_result)
			VALUES ($1, $2, $3, $4, $5)
		`, identityID, o.CheckType, o.Provider, o.Result, rawJSON); err != nil {
			return fmt.Errorf("kyc: insert check: %w", err)
		}
	}
	return nil
}

// Evaluate rolls up the individual check outcomes into an overall result
// and the resulting verification tier. Any hard failure wins over review,
// which wins over pass; tier reflects how much of the pipeline passed.
func Evaluate(outcomes []CheckOutcome) (tier int, overall Result) {
	hasFail, hasReview := false, false
	idPass, livenessPass, addressPass := false, false, false

	for _, o := range outcomes {
		switch o.Result {
		case ResultFail:
			hasFail = true
		case ResultReview:
			hasReview = true
		}
		switch o.CheckType {
		case CheckIDDocument:
			idPass = o.Result == ResultPass
		case CheckLiveness:
			livenessPass = o.Result == ResultPass
		case CheckAddress:
			addressPass = o.Result == ResultPass
		}
	}

	switch {
	case idPass && livenessPass && addressPass:
		tier = 2
	case idPass && livenessPass:
		tier = 1
	default:
		tier = 0
	}

	switch {
	case hasFail:
		overall = ResultFail
	case hasReview:
		overall = ResultReview
	default:
		overall = ResultPass
	}
	return tier, overall
}
