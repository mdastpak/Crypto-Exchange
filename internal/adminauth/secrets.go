package adminauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// FieldSealer seals values with the field-enc data keys (#146).
// *keys.Live implements it.
type FieldSealer interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
}

// Secrets are the keys that protect authenticator secrets in
// users.totp_secret (#146).
type Secrets struct {
	// LegacyKey is admin.totp_key, decoded, or nil when it is not set.
	LegacyKey []byte
	// DataKeys seals with the field-enc data keys, or is nil without
	// keys.provider.
	DataKeys FieldSealer
	// LegacyWrites seals new secrets with LegacyKey while it is set
	// (keys.legacy_secrets "write"), so that instances of a release before
	// #146, which know only admin.totp_key, can read them.
	LegacyWrites bool
}

// ErrNoKeys reports a service without any key for authenticator secrets.
var ErrNoKeys = errors.New("authenticator secrets need admin.totp_key or keys.provider")

// Format is how an authenticator secret is sealed.
type Format string

const (
	// FormatLegacy is AES-256-GCM with admin.totp_key: "v1:" and base64.
	FormatLegacy Format = "legacy"
	// FormatDataKeys is a field-enc data key, through keys.Keyring.SealField:
	// "v2:" and base64 of the key version, the nonce, and the ciphertext.
	FormatDataKeys Format = "data-keys"
)

// The prefixes that mark the format of a stored secret.
const (
	legacyPrefix   = "v1:"
	dataKeysPrefix = "v2:"
)

// formatOf returns the format of a stored secret, or false for an unknown one.
func formatOf(stored string) (Format, bool) {
	switch {
	case strings.HasPrefix(stored, legacyPrefix):
		return FormatLegacy, true
	case strings.HasPrefix(stored, dataKeysPrefix):
		return FormatDataKeys, true
	}
	return "", false
}

// sealingAAD names the place of a secret sealed with a data key: the column
// and the administrator, so that it cannot be copied to another user row.
func sealingAAD(adminID string) []byte { return []byte("users.totp_secret:" + adminID) }

// WriteFormat is the format of new secrets: the data keys, unless only
// admin.totp_key is set, or it is set and still writes.
func (s Secrets) WriteFormat() Format {
	if s.DataKeys == nil || (s.LegacyKey != nil && s.LegacyWrites) {
		return FormatLegacy
	}
	return FormatDataKeys
}

// seal seals secret for adminID in the format that new secrets use.
func (s *Service) seal(adminID string, secret []byte) (string, error) {
	return s.sealAs(s.writes, adminID, secret)
}

// sealAs seals secret for adminID in format. The administrator's ID is
// authenticated data in both formats.
func (s *Service) sealAs(format Format, adminID string, secret []byte) (string, error) {
	switch format {
	case FormatDataKeys:
		if s.dataKeys == nil {
			return "", errors.New("sealing with the data keys needs keys.provider")
		}
		sealed, err := s.dataKeys.SealField(sealingAAD(adminID), secret)
		if err != nil {
			return "", fmt.Errorf("seal the authenticator secret: %w", err)
		}
		return dataKeysPrefix + base64.StdEncoding.EncodeToString(sealed), nil
	case FormatLegacy:
		if s.legacy == nil {
			return "", errors.New("sealing with admin.totp_key needs admin.totp_key")
		}
		nonce := make([]byte, s.legacy.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		sealed := s.legacy.Seal(nonce, nonce, secret, []byte(adminID))
		return legacyPrefix + base64.StdEncoding.EncodeToString(sealed), nil
	}
	return "", fmt.Errorf("unknown format %q", format)
}

// open opens a stored secret of either format.
func (s *Service) open(adminID, stored string) ([]byte, error) {
	format, ok := formatOf(stored)
	if !ok {
		return nil, errors.New("the stored authenticator secret has an unknown format")
	}
	sealed, err := base64.StdEncoding.DecodeString(stored[len(legacyPrefix):])
	if err != nil {
		return nil, errors.New("the stored authenticator secret is malformed")
	}
	if format == FormatDataKeys {
		if s.dataKeys == nil {
			return nil, errors.New("the authenticator secret is sealed with a data key, and keys.provider is not set")
		}
		secret, err := s.dataKeys.OpenField(sealingAAD(adminID), sealed)
		if err != nil {
			return nil, fmt.Errorf("decrypt the authenticator secret: %w", err)
		}
		return secret, nil
	}
	if s.legacy == nil {
		return nil, errors.New("the authenticator secret is sealed with admin.totp_key, which is not set")
	}
	if len(sealed) < s.legacy.NonceSize() {
		return nil, errors.New("the stored authenticator secret is malformed")
	}
	nonce, ciphertext := sealed[:s.legacy.NonceSize()], sealed[s.legacy.NonceSize():]
	secret, err := s.legacy.Open(nil, nonce, ciphertext, []byte(adminID))
	if err != nil {
		// Usually a changed admin.totp_key: the administrator must be
		// enrolled again with ResetTOTP.
		return nil, fmt.Errorf("decrypt the authenticator secret: %w", err)
	}
	return secret, nil
}

// SecretCounts are the authenticator secrets of administrators by format.
type SecretCounts struct {
	Legacy   int
	DataKeys int
	// Unknown counts secrets in neither format, which no release reads.
	Unknown int
}

// CountSecrets counts the stored authenticator secrets of every
// administrator, disabled ones included, by format.
func (s *Service) CountSecrets(ctx context.Context) (SecretCounts, error) {
	return CountSecrets(ctx, s.db)
}

// CountSecrets counts the stored authenticator secrets by format, without
// any key, for a startup warning or `admin totp-secrets status`.
func CountSecrets(ctx context.Context, conn *sql.DB) (SecretCounts, error) {
	var c SecretCounts
	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE totp_secret LIKE 'v1:%'),
		       COUNT(*) FILTER (WHERE totp_secret LIKE 'v2:%'),
		       COUNT(*) FILTER (WHERE totp_secret NOT LIKE 'v1:%' AND totp_secret NOT LIKE 'v2:%')
		FROM users WHERE totp_secret IS NOT NULL`).Scan(&c.Legacy, &c.DataKeys, &c.Unknown)
	if err != nil {
		return SecretCounts{}, fmt.Errorf("count the authenticator secrets: %w", err)
	}
	return c, nil
}

// Reseal seals every stored authenticator secret that is not in format
// again in format, one administrator per transaction, and returns how many
// it changed. Each change records admin.totp_resealed, which names the
// formats, never the secret. The secrets themselves do not change, so
// enrolled authenticator apps keep working.
//
// Resealing to FormatDataKeys must wait until every instance runs a release
// that reads data keys; resealing to FormatLegacy is the way back before a
// rollback to such a release.
func (s *Service) Reseal(ctx context.Context, format Format) (int, error) {
	var prefix string
	switch format {
	case FormatLegacy:
		prefix = legacyPrefix
	case FormatDataKeys:
		prefix = dataKeysPrefix
	default:
		return 0, fmt.Errorf("unknown format %q", format)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT hk FROM users WHERE totp_secret IS NOT NULL AND totp_secret NOT LIKE $1 ORDER BY hk`, prefix+"%")
	if err != nil {
		return 0, fmt.Errorf("find the authenticator secrets: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("find the authenticator secrets: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find the authenticator secrets: %w", err)
	}

	changed := 0
	for _, id := range ids {
		done, err := s.resealOne(ctx, id, format, prefix)
		if err != nil {
			return changed, fmt.Errorf("administrator %s: %w", id, err)
		}
		if done {
			changed++
		}
	}
	return changed, nil
}

// resealOne reseals the secret of one administrator, locking the row, so a
// concurrent reset of the secret is never overwritten with the old one.
func (s *Service) resealOne(ctx context.Context, adminID string, format Format, prefix string) (bool, error) {
	done := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var stored sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT totp_secret FROM users WHERE hk = $1 FOR UPDATE`, adminID).Scan(&stored); err != nil {
			return err
		}
		if !stored.Valid || strings.HasPrefix(stored.String, prefix) {
			return nil
		}
		before, _ := formatOf(stored.String)
		secret, err := s.open(adminID, stored.String)
		if err != nil {
			return err
		}
		sealed, err := s.sealAs(format, adminID, secret)
		clear(secret)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret = $2, updated_at = NOW() WHERE hk = $1`, adminID, sealed); err != nil {
			return err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Users, "hk = $1", adminID); err != nil {
			return err
		}
		e := audit.Of(s.Origin, audit.ActionAdminTOTPResealed, audit.TargetUser, adminID)
		e.Changes = &audit.Changes{Before: map[string]interface{}{"format": string(before)}, After: map[string]interface{}{"format": string(format)}}
		if err := audit.Record(ctx, tx, e); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}
