// Package adminauth manages administrator accounts: creating them, enrolling
// their authenticator app (TOTP), and checking authenticator codes at sign-in.
// Administrators are rows of the users table whose stored role is admin, so
// admin tokens never depend on a role chosen by the client.
package adminauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/totp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidEmail = errors.New("invalid email address")
	ErrUserExists   = errors.New("a user with this email address already exists")
	ErrNotFound     = errors.New("no active administrator with this email address")
	ErrInvalidCode  = errors.New("invalid authenticator code")
	ErrInvalidKey   = errors.New("admin.totp_key must be 32 random bytes, base64-encoded")
)

// KeySize is the size of the key that encrypts TOTP secrets.
const KeySize = 32

// sealedPrefix marks the format of encrypted secrets, so that it can change.
const sealedPrefix = "v1:"

// Service manages administrator accounts.
type Service struct {
	db      *sql.DB
	secrets cipher.AEAD
	// issuer names the exchange in authenticator apps.
	issuer string
	now    func() time.Time
	// Origin is who makes the changes of Create, ResetTOTP, and Disable,
	// which record an audit event in their transaction (#195): for
	// cmd/admin, audit.Operator. Zero is the system.
	Origin audit.Event
}

// DecodeKey decodes admin.totp_key.
func DecodeKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// NewService returns a service that stores TOTP secrets encrypted with key.
// issuer names the exchange in authenticator apps.
func NewService(db *sql.DB, key []byte, issuer string) (*Service, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	secrets, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if issuer == "" {
		issuer = "Exchange"
	}
	return &Service{db: db, secrets: secrets, issuer: issuer, now: time.Now}, nil
}

// NormalizeEmail returns address in lower case, or ErrInvalidEmail when it is
// not a bare email address.
func NormalizeEmail(address string) (string, error) {
	address = strings.TrimSpace(address)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || parsed.Name != "" {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(address), nil
}

// MaskEmail hides most of the local part of an email address, for logs and
// responses.
func MaskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "*****"
	}
	visible := min(3, at)
	return email[:visible] + "*****" + email[at:]
}

// Enrollment is what an operator needs to add an administrator's account to
// an authenticator app. It is shown once and never stored in plain text.
type Enrollment struct {
	AdminID string
	Email   string
	// Secret is the TOTP secret in base32, for manual entry.
	Secret string
	// URI is the otpauth URI, usually shown as a QR code.
	URI string
}

// Admin is an active administrator.
type Admin struct {
	ID    string // users.hk
	Email string
}

// ownsEmail matches the user that owns an email address, through the unique
// index of user_contacts. Its arguments are "email" and the address.
const ownsEmail = `hk = (SELECT c.user_hk FROM user_contacts c WHERE ` + db.ContactMatch + `)`

// emailContact is the contact type of an administrator's address.
const emailContact = "email"

// Create creates an administrator with a new TOTP secret. It refuses an email
// address that already belongs to a user: an existing account is not
// silently turned into an administrator.
func (s *Service) Create(ctx context.Context, email string) (Enrollment, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Enrollment{}, err
	}
	adminID := uuid.NewString()
	secret, sealed, err := s.newSecret(adminID)
	if err != nil {
		return Enrollment{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Enrollment{}, fmt.Errorf("create the administrator: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO users (hk, status, role, totp_secret, totp_enabled)
		 VALUES ($1, 'ACTIVE', 'admin', $2, TRUE)`,
		adminID, sealed,
	)
	if err == nil {
		// The address is verified at the first sign-in, by its code; see
		// ConfirmEmail.
		err = db.InsertContact(ctx, tx, adminID, emailContact, email, false)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Enrollment{}, ErrUserExists
	}
	if err == nil {
		// The event holds the address and the role, never the secret.
		e := audit.Of(s.Origin, audit.ActionAdminCreate, audit.TargetUser, adminID)
		e.Changes = &audit.Changes{After: map[string]interface{}{"email": email, "role": "admin", "status": "ACTIVE"}}
		err = audit.Record(ctx, tx, e)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("create the administrator: %w", err)
	}
	return s.enrollment(adminID, email, secret), nil
}

// ResetTOTP replaces the TOTP secret of an administrator, for example after a
// lost device. The old secret stops working at once.
func (s *Service) ResetTOTP(ctx context.Context, email string) (Enrollment, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Enrollment{}, err
	}
	var adminID string
	err = s.db.QueryRowContext(ctx,
		`SELECT hk FROM users WHERE role = 'admin' AND `+ownsEmail, emailContact, email,
	).Scan(&adminID)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrNotFound
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("find the administrator: %w", err)
	}

	secret, sealed, err := s.newSecret(adminID)
	if err != nil {
		return Enrollment{}, err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET totp_secret = $2, totp_enabled = TRUE, totp_last_step = NULL, updated_at = NOW()
			 WHERE hk = $1 AND role = 'admin'`,
			adminID, sealed,
		); err != nil {
			return err
		}
		// The event says that the secret changed, never what it is.
		return audit.Record(ctx, tx, audit.Of(s.Origin, audit.ActionAdminTOTPReset, audit.TargetUser, adminID))
	})
	if err != nil {
		return Enrollment{}, fmt.Errorf("store the new secret: %w", err)
	}
	return s.enrollment(adminID, email, secret), nil
}

// inTx runs change in a transaction, which commits only when change, and
// the audit event it records, succeed.
func (s *Service) inTx(ctx context.Context, change func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Disable stops an administrator from signing in. Admin tokens issued
// earlier stay valid until they expire.
func (s *Service) Disable(ctx context.Context, email string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var adminID, before string
		err := tx.QueryRowContext(ctx,
			`UPDATE users u SET status = 'INACTIVE', updated_at = NOW()
			 FROM (SELECT hk, status FROM users WHERE role = 'admin' AND `+ownsEmail+` FOR UPDATE) old
			 WHERE u.hk = old.hk
			 RETURNING u.hk, old.status`, emailContact, email,
		).Scan(&adminID, &before)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("disable the administrator: %w", err)
		}
		e := audit.Of(s.Origin, audit.ActionAdminDisable, audit.TargetUser, adminID)
		e.Changes = &audit.Changes{Before: map[string]interface{}{"status": before}, After: map[string]interface{}{"status": "INACTIVE"}}
		if err := audit.Record(ctx, tx, e); err != nil {
			return fmt.Errorf("disable the administrator: %w", err)
		}
		return nil
	})
}

// FindActive returns the administrator with email who may sign in: role
// admin, status ACTIVE, not deleted, and an enrolled authenticator.
func (s *Service) FindActive(ctx context.Context, email string) (Admin, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Admin{}, ErrNotFound
	}
	var admin Admin
	err = s.db.QueryRowContext(ctx,
		`SELECT hk FROM users
		 WHERE role = 'admin' AND status = 'ACTIVE' AND totp_enabled AND deleted_at IS NULL AND `+ownsEmail,
		emailContact, email,
	).Scan(&admin.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	if err != nil {
		return Admin{}, fmt.Errorf("find the administrator: %w", err)
	}
	admin.Email = email
	return admin, nil
}

// IsActive reports whether adminID is still an administrator who may sign in.
// Admin requests check it, so demoting or disabling an administrator, also
// directly in the database, revokes its tokens at once.
func (s *Service) IsActive(ctx context.Context, adminID string) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users
		 WHERE hk = $1 AND role = 'admin' AND status = 'ACTIVE' AND totp_enabled AND deleted_at IS NULL)`,
		adminID,
	).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check the administrator: %w", err)
	}
	return active, nil
}

// VerifyTOTP checks an authenticator code of an active administrator. Each
// code is accepted once: the matched time step must be later than the last
// accepted one, which the update checks atomically.
func (s *Service) VerifyTOTP(ctx context.Context, adminID, code string) error {
	var sealed sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT totp_secret FROM users
		 WHERE hk = $1 AND role = 'admin' AND status = 'ACTIVE' AND totp_enabled AND deleted_at IS NULL`,
		adminID,
	).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !sealed.Valid) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load the authenticator secret: %w", err)
	}
	secret, err := s.open(adminID, sealed.String)
	if err != nil {
		return err
	}

	step, ok := totp.Verify(secret, code, s.now())
	if !ok {
		return ErrInvalidCode
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_last_step = $2
		 WHERE hk = $1 AND (totp_last_step IS NULL OR totp_last_step < $2)`,
		adminID, step,
	)
	if err != nil {
		return fmt.Errorf("record the authenticator code: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		// The code, or a later one, was already used.
		return ErrInvalidCode
	}
	return nil
}

// ConfirmEmail records that an administrator has proven control of their
// email address, by the code of a successful sign-in. The first sign-in
// verifies the address that the operator entered.
func (s *Service) ConfirmEmail(ctx context.Context, adminID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_contacts c SET verified_at = NOW(), updated_at = NOW()
		 WHERE user_hk = $1 AND type = 'email' AND verified_at IS NULL AND c.removed_at IS NULL`,
		adminID,
	)
	if err != nil {
		return fmt.Errorf("confirm the email address: %w", err)
	}
	return nil
}

func (s *Service) enrollment(adminID, email string, secret []byte) Enrollment {
	return Enrollment{
		AdminID: adminID,
		Email:   email,
		Secret:  totp.EncodeSecret(secret),
		URI:     totp.ProvisioningURI(s.issuer, email, secret),
	}
}

// newSecret generates a TOTP secret and seals it for adminID.
func (s *Service) newSecret(adminID string) ([]byte, string, error) {
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, "", err
	}
	sealed, err := s.seal(adminID, secret)
	if err != nil {
		return nil, "", err
	}
	return secret, sealed, nil
}

// seal encrypts secret with AES-GCM. The administrator's ID is authenticated
// data, so a secret cannot be copied to another administrator.
func (s *Service) seal(adminID string, secret []byte) (string, error) {
	nonce := make([]byte, s.secrets.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	sealed := s.secrets.Seal(nonce, nonce, secret, []byte(adminID))
	return sealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Service) open(adminID, stored string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(stored, sealedPrefix)
	if !ok {
		return nil, errors.New("the stored authenticator secret has an unknown format")
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(sealed) < s.secrets.NonceSize() {
		return nil, errors.New("the stored authenticator secret is malformed")
	}
	nonce, ciphertext := sealed[:s.secrets.NonceSize()], sealed[s.secrets.NonceSize():]
	secret, err := s.secrets.Open(nil, nonce, ciphertext, []byte(adminID))
	if err != nil {
		// Usually a changed admin.totp_key: the administrator must be
		// enrolled again with ResetTOTP.
		return nil, fmt.Errorf("decrypt the authenticator secret: %w", err)
	}
	return secret, nil
}
