package resilience

import (
	"context"
	"fmt"
)

const killSwitchColumns = `id::text, scope_type, scope_value, enabled, reason, updated_by, created_at, updated_at`

// ListKillSwitches returns every kill switch (tripped or not), newest first.
func (s *Service) ListKillSwitches(ctx context.Context) ([]KillSwitch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+killSwitchColumns+` FROM kill_switches ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("resilience: list kill switches: %w", err)
	}
	defer rows.Close()

	var out []KillSwitch
	for rows.Next() {
		ks, err := scanKillSwitch(rows)
		if err != nil {
			return nil, fmt.Errorf("resilience: scan kill switch: %w", err)
		}
		out = append(out, ks)
	}
	return out, rows.Err()
}

// SetKillSwitch creates or updates the switch for scope. Invalidates the
// in-process cache immediately, so the calling process's own next Check
// sees the change without waiting out cacheTTL.
func (s *Service) SetKillSwitch(ctx context.Context, scope Scope, enabled bool, reason, updatedBy string) (KillSwitch, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO kill_switches (scope_type, scope_value, enabled, reason, updated_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (scope_type, scope_value)
		DO UPDATE SET enabled = EXCLUDED.enabled, reason = EXCLUDED.reason, updated_by = EXCLUDED.updated_by
		RETURNING `+killSwitchColumns, scope.Type, scope.Value, enabled, nullableString(reason), updatedBy)
	ks, err := scanKillSwitch(row)
	if err != nil {
		return KillSwitch{}, fmt.Errorf("resilience: set kill switch: %w", err)
	}
	s.invalidate()
	return ks, nil
}

// GetSystemMode returns the current singleton read-only-mode state.
func (s *Service) GetSystemMode(ctx context.Context) (SystemMode, error) {
	var m SystemMode
	var reason, updatedBy *string
	err := s.pool.QueryRow(ctx, `SELECT read_only, reason, updated_by, updated_at FROM system_mode WHERE id = true`).
		Scan(&m.ReadOnly, &reason, &updatedBy, &m.UpdatedAt)
	if err != nil {
		return SystemMode{}, fmt.Errorf("resilience: get system mode: %w", err)
	}
	if reason != nil {
		m.Reason = *reason
	}
	if updatedBy != nil {
		m.UpdatedBy = *updatedBy
	}
	return m, nil
}

// SetReadOnly enters or exits read-only mode. Invalidates the in-process
// cache immediately, same reasoning as SetKillSwitch.
func (s *Service) SetReadOnly(ctx context.Context, readOnly bool, reason, updatedBy string) (SystemMode, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE system_mode SET read_only = $1, reason = $2, updated_by = $3 WHERE id = true
	`, readOnly, nullableString(reason), updatedBy); err != nil {
		return SystemMode{}, fmt.Errorf("resilience: set read-only mode: %w", err)
	}
	s.invalidate()
	return s.GetSystemMode(ctx)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKillSwitch(row rowScanner) (KillSwitch, error) {
	var ks KillSwitch
	var reason *string
	err := row.Scan(&ks.ID, &ks.Scope.Type, &ks.Scope.Value, &ks.Enabled, &reason, &ks.UpdatedBy, &ks.CreatedAt, &ks.UpdatedAt)
	if err != nil {
		return KillSwitch{}, err
	}
	if reason != nil {
		ks.Reason = *reason
	}
	return ks, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
