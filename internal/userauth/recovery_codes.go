package userauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Recovery codes (#307). A user who forgot the account password proves the
// account with a code sent to a contact and one recovery code, and sets a
// new password. The codes are random, so a plain hash protects them: no
// slow function is needed to keep a stolen table from giving them away.

const (
	// RecoveryCodeCount is how many codes a user has at once.
	RecoveryCodeCount = 10
	// recoveryCodeLength is the length of a code without its dashes: 16
	// characters of 32 are 80 bits.
	recoveryCodeLength = 16
	// recoveryAlphabet has no 0, O, 1, or I, which are easy to mistake
	// when a code is copied from paper.
	recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// maxRecoveryCodeInput bounds what a request may send as a code.
	maxRecoveryCodeInput = 64
)

// ErrWrongRecoveryCode reports a recovery code that is not an unused code
// of the account.
var ErrWrongRecoveryCode = errors.New("the recovery code is wrong or already used")

// newRecoveryCode returns a random code, in groups of four.
func newRecoveryCode() string {
	raw := make([]byte, recoveryCodeLength)
	_, _ = rand.Read(raw) // never fails since Go 1.24
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		// 256 is a multiple of 32, so every character is as likely.
		b.WriteByte(recoveryAlphabet[int(v)%len(recoveryAlphabet)])
	}
	return b.String()
}

// normalizeRecoveryCode returns the code as it is hashed: upper case,
// without dashes or spaces. It reports false for text that cannot be a
// code.
func normalizeRecoveryCode(input string) (string, bool) {
	if len(input) > maxRecoveryCodeInput {
		return "", false
	}
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(input))
	if len(code) != recoveryCodeLength {
		return "", false
	}
	for _, r := range code {
		if !strings.ContainsRune(recoveryAlphabet, r) {
			return "", false
		}
	}
	return code, true
}

// hashRecoveryCode returns the stored form of a normalized code. The user
// is part of it, so a hash copied to another account matches nothing.
func hashRecoveryCode(userHK, code string) string {
	sum := sha256.Sum256([]byte("crypto-exchange/recovery-code/1\x00" + strings.ToLower(userHK) + "\x00" + code))
	return hex.EncodeToString(sum[:])
}

// execer runs statements: the transaction of a change.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// replaceRecoveryCodes retires the unused codes of a user and stores new
// ones, in the transaction of the change that calls for them. It returns
// the new codes, which are shown once and never stored.
func replaceRecoveryCodes(ctx context.Context, tx execer, userHK string) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE user_recovery_codes SET retired_at = NOW(), retired_reason = 'replaced'
		WHERE user_hk = $1 AND retired_at IS NULL`, userHK); err != nil {
		return nil, fmt.Errorf("retire the recovery codes: %w", err)
	}
	codes := make([]string, 0, RecoveryCodeCount)
	hashes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		code := newRecoveryCode()
		normalized, _ := normalizeRecoveryCode(code)
		codes, hashes = append(codes, code), append(hashes, hashRecoveryCode(userHK, normalized))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_recovery_codes (user_hk, code_hash)
		SELECT $1, h FROM unnest($2::text[]) AS h`, userHK, hashes); err != nil {
		return nil, fmt.Errorf("store the recovery codes: %w", err)
	}
	return codes, nil
}

// RecoveryCodesLeft returns how many unused recovery codes a user has.
func (p *Passwords) RecoveryCodesLeft(ctx context.Context, q Querier, userHK string) (int, error) {
	var left int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_recovery_codes WHERE user_hk = $1 AND retired_at IS NULL`,
		userHK).Scan(&left); err != nil {
		return 0, fmt.Errorf("count the recovery codes: %w", err)
	}
	return left, nil
}
