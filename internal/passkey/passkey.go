// Package passkey keeps the passkeys of users (#307): WebAuthn credentials,
// such as Face ID, a fingerprint, Windows Hello, or a security key, that
// prove the second step of a sign-in instead of the account password.
//
// The credentials are in PostgreSQL, sealed like the account (#139), and
// the state of a ceremony in progress is in Redis, so any instance finishes
// a ceremony that another one started.
package passkey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/userauth"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// ceremonyTTL is how long a registration or an assertion can be
	// finished after it began.
	ceremonyTTL = 5 * time.Minute
	// MaxPerUser is how many passkeys an account can have.
	MaxPerUser = 10
	// MaxNameLength is the most characters of the name of a passkey.
	MaxNameLength = 64

	registrationKeyPrefix = "passkey_registration:"
	assertionKeyPrefix    = "passkey_assertion:"
)

// The refusals of the passkeys.
var (
	// ErrDisabled reports a deployment without security.passkeys.rp_id.
	ErrDisabled = errors.New("passkeys are not configured")
	// ErrNoPasskey reports an account without a passkey, or a passkey
	// that is not the account's.
	ErrNoPasskey = errors.New("the account has no such passkey")
	// ErrNoCeremony reports a response without a ceremony that waits for
	// it: none began, it expired, or it was already answered.
	ErrNoCeremony = errors.New("no passkey ceremony is in progress")
	// ErrInvalid reports a response that does not prove a passkey of the
	// account.
	ErrInvalid = errors.New("the passkey response is not valid")
	// ErrTooMany reports an account that has MaxPerUser passkeys.
	ErrTooMany = errors.New("the account has the most passkeys allowed")
	// ErrExists reports a credential that is already a passkey.
	ErrExists = errors.New("the credential is already registered")
	// ErrCloned reports a credential whose signature counter went back:
	// a copy of it may exist. It proves nothing.
	ErrCloned = errors.New("the passkey may have been cloned")
)

// Config is the relying party of the passkeys.
type Config struct {
	// RPID is the domain that the passkeys are bound to. Empty turns the
	// passkeys off.
	RPID string
	// RPName names the exchange in the prompt.
	RPName string
	// Origins are the origins of the clients.
	Origins []string
}

// Service registers and checks the passkeys of users.
type Service struct {
	db    *sql.DB
	redis redis.Cmdable
	wa    *webauthn.WebAuthn
}

// New returns the service. Without cfg.RPID it is disabled: every ceremony
// answers ErrDisabled, and no account has a passkey to prove.
func New(db *sql.DB, rdb redis.Cmdable, cfg Config) (*Service, error) {
	s := &Service{db: db, redis: rdb}
	if cfg.RPID == "" {
		return s, nil
	}
	name := cfg.RPName
	if name == "" {
		name = "Exchange"
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: name,
		RPOrigins:     cfg.Origins,
		// The exchange does not need to know the make of an authenticator.
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementPreferred,
			// The passkey is a second factor on its own only when the
			// device checks its holder: a biometric or a PIN.
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure the passkeys: %w", err)
	}
	s.wa = wa
	return s, nil
}

// Enabled reports whether passkeys are configured.
func (s *Service) Enabled() bool { return s != nil && s.wa != nil }

// Passkey is a passkey of a user, without its key.
type Passkey struct {
	ID   string `json:"id"`
	Name string `json:"name" example:"iPhone"`
	// Synced reports a passkey that its provider backs up and syncs to
	// the other devices of the user.
	Synced     bool       `json:"synced"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// stored is a passkey with its credential.
type stored struct {
	Passkey
	credential webauthn.Credential
}

// owner is the user of a ceremony, as the library reads it.
type owner struct {
	hk          uuid.UUID
	name        string
	credentials []webauthn.Credential
}

func (o owner) WebAuthnID() []byte                         { return o.hk[:] }
func (o owner) WebAuthnName() string                       { return o.name }
func (o owner) WebAuthnDisplayName() string                { return o.name }
func (o owner) WebAuthnCredentials() []webauthn.Credential { return o.credentials }

// querier reads rows: a *sql.DB or a *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// load reads the passkeys of a user. A row that does not match its seal was
// written outside the backend: it is left out, so it proves nothing.
func load(ctx context.Context, q querier, userHK string) ([]stored, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, sign_count, created_at, last_used_at, `+rowseal.UserPasskeys.Fields("")+`
		FROM user_passkeys WHERE user_hk = $1 AND removed_at IS NULL ORDER BY created_at, id`, userHK)
	if err != nil {
		return nil, fmt.Errorf("read the passkeys: %w", err)
	}
	defer rows.Close()
	var out []stored
	for rows.Next() {
		var p stored
		var signCount int64
		var lastUsed sql.NullTime
		row := rowseal.UserPasskeys.NewRow()
		dest := append([]any{&p.Name, &signCount, &p.CreatedAt, &lastUsed}, row.Dest()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("read a passkey: %w", err)
		}
		if err := rowseal.Check(ctx, rowseal.UserPasskeys, row); err != nil {
			if errors.Is(err, rowseal.ErrUnverified) {
				continue
			}
			return nil, err
		}
		if err := json.Unmarshal([]byte(row.Values[2].String), &p.credential); err != nil {
			return nil, fmt.Errorf("read the credential of a passkey: %w", err)
		}
		p.ID = row.Key
		p.credential.Authenticator.SignCount = uint32(min(signCount, int64(^uint32(0)))) //nolint:gosec // G115: bounded by min.
		p.Synced = p.credential.Flags.BackupState
		if lastUsed.Valid {
			p.LastUsedAt = &lastUsed.Time
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the passkeys: %w", err)
	}
	return out, nil
}

func (s *Service) owner(ctx context.Context, userHK, name string) (owner, []stored, error) {
	hk, err := uuid.Parse(userHK)
	if err != nil {
		return owner{}, nil, fmt.Errorf("the user of a passkey: %w", err)
	}
	passkeys, err := load(ctx, s.db, userHK)
	if err != nil {
		return owner{}, nil, err
	}
	o := owner{hk: hk, name: name}
	for _, p := range passkeys {
		o.credentials = append(o.credentials, p.credential)
	}
	return o, passkeys, nil
}

// List returns the passkeys of a user, oldest first.
func (s *Service) List(ctx context.Context, userHK string) ([]Passkey, error) {
	passkeys, err := load(ctx, s.db, userHK)
	if err != nil {
		return nil, err
	}
	out := make([]Passkey, len(passkeys))
	for i, p := range passkeys {
		out[i] = p.Passkey
	}
	return out, nil
}

// Count returns how many passkeys a user can prove. It is zero while the
// passkeys are disabled.
func (s *Service) Count(ctx context.Context, userHK string) (int, error) {
	if !s.Enabled() {
		return 0, nil
	}
	passkeys, err := load(ctx, s.db, userHK)
	return len(passkeys), err
}

// saveCeremony keeps the state of a ceremony for its answer.
func (s *Service) saveCeremony(ctx context.Context, key string, session *webauthn.SessionData) error {
	raw, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode the passkey ceremony: %w", err)
	}
	if err := s.redis.Set(ctx, key, raw, ceremonyTTL).Err(); err != nil {
		return fmt.Errorf("store the passkey ceremony: %w", err)
	}
	return nil
}

// takeCeremony returns the state of a ceremony, once: of two answers to it,
// one finds it.
func (s *Service) takeCeremony(ctx context.Context, key string) (webauthn.SessionData, error) {
	var session webauthn.SessionData
	raw, err := s.redis.GetDel(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return session, ErrNoCeremony
	}
	if err != nil {
		return session, fmt.Errorf("read the passkey ceremony: %w", err)
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return session, ErrNoCeremony
	}
	return session, nil
}

// BeginRegistration starts adding a passkey to a user, and returns the
// options for navigator.credentials.create. account names the user in the
// prompt. A new call replaces a registration that was not finished.
func (s *Service) BeginRegistration(ctx context.Context, userHK, account string) (*protocol.CredentialCreation, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	o, passkeys, err := s.owner(ctx, userHK, account)
	if err != nil {
		return nil, err
	}
	if len(passkeys) >= MaxPerUser {
		return nil, ErrTooMany
	}
	// The device refuses a credential that the account already has.
	exclude := make([]protocol.CredentialDescriptor, len(o.credentials))
	for i, c := range o.credentials {
		exclude[i] = c.Descriptor()
	}
	creation, session, err := s.wa.BeginRegistration(o, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, fmt.Errorf("begin the registration of a passkey: %w", err)
	}
	if err := s.saveCeremony(ctx, registrationKeyPrefix+userHK, session); err != nil {
		return nil, err
	}
	return creation, nil
}

// FinishRegistration checks the answer of the device to BeginRegistration
// and stores the passkey under name. It is a change of the second factor,
// so the security hold starts; it returns when the hold ends.
func (s *Service) FinishRegistration(ctx context.Context, userHK, name string, response []byte, event audit.Event) (Passkey, time.Time, error) {
	none := time.Time{}
	if !s.Enabled() {
		return Passkey{}, none, ErrDisabled
	}
	name = strings.TrimSpace(name)
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return Passkey{}, none, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	session, err := s.takeCeremony(ctx, registrationKeyPrefix+userHK)
	if err != nil {
		return Passkey{}, none, err
	}
	o, _, err := s.owner(ctx, userHK, "")
	if err != nil {
		return Passkey{}, none, err
	}
	credential, err := s.wa.CreateCredential(o, session, parsed)
	if err != nil {
		return Passkey{}, none, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return Passkey{}, none, fmt.Errorf("encode the credential of a passkey: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Passkey{}, none, fmt.Errorf("start the registration of a passkey: %w", err)
	}
	defer tx.Rollback()
	// The user row orders this with the other changes of the account, and
	// makes the count below exact.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE hk = $1 FOR UPDATE`, userHK); err != nil {
		return Passkey{}, none, fmt.Errorf("lock the user: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_passkeys WHERE user_hk = $1 AND removed_at IS NULL`, userHK).Scan(&count); err != nil {
		return Passkey{}, none, fmt.Errorf("count the passkeys: %w", err)
	}
	if count >= MaxPerUser {
		return Passkey{}, none, ErrTooMany
	}
	p := Passkey{ID: uuid.NewString(), Name: name, Synced: credential.Flags.BackupState}
	err = tx.QueryRowContext(ctx, `INSERT INTO user_passkeys (id, user_hk, credential_id, credential, name, sign_count)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (credential_id) WHERE removed_at IS NULL DO NOTHING RETURNING created_at`,
		p.ID, userHK, credential.ID, string(raw), name, int64(credential.Authenticator.SignCount)).Scan(&p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Passkey{}, none, ErrExists
	}
	if err != nil {
		return Passkey{}, none, fmt.Errorf("store the passkey: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasskeys, "id = $1", p.ID); err != nil {
		return Passkey{}, none, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, userauth.HoldDuration())
	if err != nil {
		return Passkey{}, none, err
	}
	event.Action, event.Outcome = audit.ActionUserPasskeyAdd, audit.Success
	event.TargetType, event.TargetID = audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return Passkey{}, none, fmt.Errorf("record the passkey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Passkey{}, none, fmt.Errorf("store the passkey: %w", err)
	}
	return p, holdUntil, nil
}

// Remove removes a passkey of a user. It is a change of the second factor,
// so the security hold starts.
func (s *Service) Remove(ctx context.Context, userHK, id string, event audit.Event) (time.Time, error) {
	none := time.Time{}
	if _, err := uuid.Parse(id); err != nil {
		return none, ErrNoPasskey
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return none, fmt.Errorf("start the removal of a passkey: %w", err)
	}
	defer tx.Rollback()
	// The row stays, marked (#248), and its seal covers the mark.
	result, err := tx.ExecContext(ctx, `UPDATE user_passkeys SET removed_at = NOW()
		WHERE id = $1 AND user_hk = $2 AND removed_at IS NULL`, id, userHK)
	if err != nil {
		return none, fmt.Errorf("remove the passkey: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return none, ErrNoPasskey
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.UserPasskeys, "id = $1", id); err != nil {
		return none, err
	}
	holdUntil, err := restrictions.StartSecurityHold(ctx, tx, userHK, userauth.HoldDuration())
	if err != nil {
		return none, err
	}
	event.Action, event.Outcome = audit.ActionUserPasskeyRemove, audit.Success
	event.TargetType, event.TargetID = audit.TargetUser, userHK
	if err := audit.Record(ctx, tx, event); err != nil {
		return none, fmt.Errorf("record the removal of the passkey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return none, fmt.Errorf("remove the passkey: %w", err)
	}
	return holdUntil, nil
}

// assertionKey is the Redis key of the assertion of a scope: a sign-in
// challenge, or a session. Only its hash is stored.
func assertionKey(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return assertionKeyPrefix + hex.EncodeToString(sum[:])
}

// BeginAssertion starts proving a passkey of a user, and returns the
// options for navigator.credentials.get. scope names what the proof is for,
// and FinishAssertion takes the same scope: a proof that began for one
// sign-in or session finishes no other.
func (s *Service) BeginAssertion(ctx context.Context, userHK, scope string) (*protocol.CredentialAssertion, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	o, passkeys, err := s.owner(ctx, userHK, "")
	if err != nil {
		return nil, err
	}
	if len(passkeys) == 0 {
		return nil, ErrNoPasskey
	}
	assertion, session, err := s.wa.BeginLogin(o, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, fmt.Errorf("begin the assertion of a passkey: %w", err)
	}
	if err := s.saveCeremony(ctx, assertionKey(scope), session); err != nil {
		return nil, err
	}
	return assertion, nil
}

// FinishAssertion checks the answer of the device to BeginAssertion. A
// right answer proves the owner of the account, so it cancels an account
// recovery, as the password does.
func (s *Service) FinishAssertion(ctx context.Context, userHK, scope string, response []byte) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	session, err := s.takeCeremony(ctx, assertionKey(scope))
	if err != nil {
		return err
	}
	o, passkeys, err := s.owner(ctx, userHK, "")
	if err != nil {
		return err
	}
	credential, err := s.wa.ValidateLogin(o, session, parsed)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if credential.Authenticator.CloneWarning {
		return ErrCloned
	}
	var id string
	for _, p := range passkeys {
		if bytes.Equal(p.credential.ID, credential.ID) {
			id = p.ID
		}
	}
	if id == "" {
		return ErrInvalid
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record the use of a passkey: %w", err)
	}
	defer tx.Rollback()
	// A passkey that was removed since it was read proves nothing.
	result, err := tx.ExecContext(ctx, `UPDATE user_passkeys SET sign_count = $2, last_used_at = NOW()
		WHERE id = $1 AND removed_at IS NULL`, id, int64(credential.Authenticator.SignCount))
	if err != nil {
		return fmt.Errorf("record the use of a passkey: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return ErrInvalid
	}
	if err := userauth.CancelRecoveryFor(ctx, tx, userHK); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record the use of a passkey: %w", err)
	}
	return nil
}

// Options documents the answer that begins a ceremony: the options that the
// client hands to navigator.credentials.create or navigator.credentials.get.
type Options struct {
	// PublicKey holds the options, as the WebAuthn API defines them.
	PublicKey map[string]any `json:"publicKey"`
}
