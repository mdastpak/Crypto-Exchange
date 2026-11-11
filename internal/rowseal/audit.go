package rowseal

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// The Sealer seals the audit events (#337) for the audit package, which
// cannot import this one.
var _ audit.Sealer = (*Sealer)(nil)

// EventFields returns the expressions of the canonical text of the sealed
// columns of AuditEvents, qualified by alias.
func (s *Sealer) EventFields(alias string) []string { return AuditEvents.ColumnFields(alias) }

// SealEvent returns the seal of a new audit event, whose sealed columns have
// the canonical text values, and the version of the current row-mac key.
func (s *Sealer) SealEvent(hk string, values []sql.NullString) ([]byte, int64, error) {
	key, err := s.keys.Current(keys.PurposeRowMAC)
	if err != nil {
		return nil, 0, fmt.Errorf("seal the audit event: %w", err)
	}
	seal, err := mac(key, AuditEvents, &Row{Key: hk, Values: values})
	if err != nil {
		return nil, 0, err
	}
	return seal, int64(key.Version()), nil
}

// CheckEvent compares an audit event that was read with its seal. It
// returns "" when they match, and otherwise CheckMissing, CheckMismatch,
// or CheckUnknownKey. It records no finding: a finding is resolved by a
// re-seal, which an event, never written again, cannot have.
func (s *Sealer) CheckEvent(ctx context.Context, hk string, values []sql.NullString, seal []byte, keyVersion sql.NullInt64) (string, error) {
	v, err := s.verdict(ctx, AuditEvents, &Row{Key: hk, Values: values, Seal: seal, KeyVersion: keyVersion})
	return string(v), err
}

// SealsWithKey returns the name of a sealed table that still has rows sealed
// with version of the row-mac key, or "" when none has. Retiring such a
// version leaves those rows unverifiable. The verifier moves the rows of
// Tables to the current version over time, but audit events are never
// sealed again, so a version that seals one stays in use for good.
func SealsWithKey(ctx context.Context, q Querier, version int) (string, error) {
	for _, t := range append(slices.Clone(Tables), AuditEvents) {
		var used bool
		// The table name is a constant of this package; the version is an
		// argument.
		query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE seal_key_version = $1)`, t.Name)
		if err := q.QueryRowContext(ctx, query, version).Scan(&used); err != nil {
			return "", fmt.Errorf("check the seals of %s: %w", t.Name, err)
		}
		if used {
			return t.Name, nil
		}
	}
	return "", nil
}
