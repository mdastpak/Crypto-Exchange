package otp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

// Errors returned by Store. Callers map them to responses without revealing
// more than the error says.
var (
	// ErrInvalidCode reports a wrong code, or a session that does not exist,
	// has expired, or was already used.
	ErrInvalidCode = errors.New("invalid or expired code")
	// ErrTooManyAttempts reports that the attempts of the session or of its
	// contact are used up.
	ErrTooManyAttempts = errors.New("too many failed attempts")
	// ErrTooManyRequests reports that the contact received too many codes in
	// the current hour.
	ErrTooManyRequests = errors.New("too many codes requested for this contact")
)

// contactWindow is the period of the per-contact limits. It starts with the
// first request or attempt and is not extended by later ones.
const contactWindow = time.Hour

// Redis layout. Keys never contain a code or a raw contact.
const (
	sessionKeyPrefix = "otp:session:"
	contactKeyPrefix = "otp:contact:"

	fieldCode      = "code"       // HMAC of the session ID and the code
	fieldContact   = "contact"    // the contact, encrypted
	fieldContactID = "contact_id" // HMAC of the normalized contact
	fieldAttempts  = "attempts"
)

// Store keeps sign-in sessions in Redis. A session holds an HMAC of its code,
// never the code itself, and the contact encrypted with a key derived from the
// secret, so reading Redis reveals neither.
type Store struct {
	client   *redis.Client
	settings Settings
	macKey   []byte
	contacts cipher.AEAD
}

// NewStore returns a store with the given settings. Without a secret it uses
// a random one, which only works while a single process issues and verifies
// the codes; Settings.Validate refuses that in release mode.
func NewStore(client *redis.Client, settings Settings) *Store {
	settings = settings.withDefaults()
	secret := []byte(settings.Secret)
	if len(secret) == 0 {
		secret = make([]byte, MinSecretLength)
		_, _ = rand.Read(secret) // never fails since Go 1.24
	}

	block, err := aes.NewCipher(deriveKey(secret, "otp contact encryption"))
	if err != nil {
		panic(err) // unreachable: the derived key is always 32 bytes
	}
	contacts, err := cipher.NewGCM(block)
	if err != nil {
		panic(err) // unreachable for AES
	}
	return &Store{
		client:   client,
		settings: settings,
		macKey:   deriveKey(secret, "otp code and contact hashing"),
		contacts: contacts,
	}
}

// deriveKey derives an independent 32-byte key for each purpose.
func deriveKey(secret []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

func (s *Store) mac(parts ...string) []byte {
	mac := hmac.New(sha256.New, s.macKey)
	mac.Write([]byte(strings.Join(parts, "\x00")))
	return mac.Sum(nil)
}

// contactID identifies a contact in Redis keys. Email addresses are matched
// case-insensitively, like the users table does.
func (s *Store) contactID(contact string) string {
	return hex.EncodeToString(s.mac("contact", strings.ToLower(contact)))
}

func sessionKey(sessionID string) string { return sessionKeyPrefix + sessionID }

func contactRequestsKey(contactID string) string { return contactKeyPrefix + contactID + ":requests" }

func contactAttemptsKey(contactID string) string { return contactKeyPrefix + contactID + ":attempts" }

// Issued is a new sign-in session.
type Issued struct {
	SessionID string
	Code      string
}

// issueScript counts a code request for a contact. It refuses the request when
// the contact has used up its attempts, so that no code is sent that could
// not be verified, or when the contact has received too many codes.
//
// KEYS[1]: contact requests, KEYS[2]: contact attempts
// ARGV[1]: requests per hour, ARGV[2]: attempts per hour, ARGV[3]: window in seconds
// Returns 1 when allowed, -1 when there are too many requests, and -2 when
// there are too many attempts.
var issueScript = redis.NewScript(`
local attempts = tonumber(redis.call('GET', KEYS[2]) or '0')
if attempts >= tonumber(ARGV[2]) then
	return -2
end
local requests = redis.call('INCR', KEYS[1])
if requests == 1 then
	redis.call('EXPIRE', KEYS[1], ARGV[3])
end
if requests > tonumber(ARGV[1]) then
	return -1
end
return 1
`)

// Issue creates a session with a new code for contact.
func (s *Store) Issue(ctx context.Context, contact string) (Issued, error) {
	contactID := s.contactID(contact)
	result, err := issueScript.Run(ctx, s.client,
		[]string{contactRequestsKey(contactID), contactAttemptsKey(contactID)},
		s.settings.ContactRequestsPerHour, s.settings.ContactAttemptsPerHour, int(contactWindow/time.Second),
	).Int()
	if err != nil {
		return Issued{}, fmt.Errorf("count the code request: %w", err)
	}
	switch result {
	case -1:
		return Issued{}, ErrTooManyRequests
	case -2:
		return Issued{}, ErrTooManyAttempts
	}

	code, err := GenerateCode(s.settings.CodeLength)
	if err != nil {
		return Issued{}, err
	}
	sessionID := uuid.NewString()
	encrypted, err := s.encryptContact(sessionID, contact)
	if err != nil {
		return Issued{}, err
	}

	key := sessionKey(sessionID)
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key,
			fieldCode, hex.EncodeToString(s.mac("code", sessionID, code)),
			fieldContact, encrypted,
			fieldContactID, contactID,
			fieldAttempts, 0,
		)
		pipe.Expire(ctx, key, s.settings.TTL)
		return nil
	})
	if err != nil {
		return Issued{}, fmt.Errorf("store the session: %w", err)
	}
	return Issued{SessionID: sessionID, Code: code}, nil
}

// attemptScript reserves one verification attempt, for the session and for its
// contact, and returns what is needed to check the code. A session whose code
// field is gone is locked: its attempts are used up.
//
// KEYS[1]: session, KEYS[2]: contact attempts
// ARGV[1]: attempts per code, ARGV[2]: attempts per contact, ARGV[3]: window in seconds
// Returns {0} when the session does not exist, {-1} when the attempts are used
// up, and otherwise {attempt number, code HMAC, encrypted contact}.
var attemptScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[1], 'code') == 0 then
	if redis.call('EXISTS', KEYS[1]) == 0 then
		return {0}
	end
	return {-1}
end
local attempt = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
if attempt > tonumber(ARGV[1]) then
	redis.call('HDEL', KEYS[1], 'code', 'contact')
	return {-1}
end
local contactAttempts = redis.call('INCR', KEYS[2])
if contactAttempts == 1 then
	redis.call('EXPIRE', KEYS[2], ARGV[3])
end
if contactAttempts > tonumber(ARGV[2]) then
	return {-1}
end
return {attempt, redis.call('HGET', KEYS[1], 'code'), redis.call('HGET', KEYS[1], 'contact')}
`)

// Verify checks code against the session and returns its contact. A correct
// code ends the session, so it works only once, and resets the attempts of
// the contact. The attempt is counted before the code is checked, so
// concurrent attempts can never exceed the limits.
func (s *Store) Verify(ctx context.Context, sessionID, code string) (string, error) {
	key := sessionKey(sessionID)
	contactID, err := s.client.HGet(ctx, key, fieldContactID).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrInvalidCode
	}
	if err != nil {
		return "", fmt.Errorf("load the session: %w", err)
	}

	attempt, storedMAC, encrypted, err := s.reserveAttempt(ctx, key, contactID)
	if err != nil {
		return "", err
	}

	expected, err := hex.DecodeString(storedMAC)
	if err != nil {
		return "", fmt.Errorf("decode the stored code: %w", err)
	}
	if !hmac.Equal(expected, s.mac("code", sessionID, code)) {
		if attempt >= int64(s.settings.AttemptsPerCode) {
			// The last attempt failed: erase the code and the contact now.
			// The session stays until it expires, so later attempts are
			// refused as too many rather than as unknown.
			if err := s.client.HDel(ctx, key, fieldCode, fieldContact).Err(); err != nil {
				return "", fmt.Errorf("lock the session: %w", err)
			}
		}
		return "", ErrInvalidCode
	}

	// Only the request that deletes the session may use the code.
	deleted, err := s.client.Del(ctx, key).Result()
	if err != nil {
		return "", fmt.Errorf("end the session: %w", err)
	}
	if deleted == 0 {
		return "", ErrInvalidCode
	}
	if err := s.client.Del(ctx, contactAttemptsKey(contactID)).Err(); err != nil {
		return "", fmt.Errorf("reset the contact attempts: %w", err)
	}
	return s.decryptContact(sessionID, encrypted)
}

// reserveAttempt counts one attempt for the session and its contact, and
// returns the attempt number with the stored code HMAC and encrypted contact.
func (s *Store) reserveAttempt(ctx context.Context, key, contactID string) (attempt int64, storedMAC, encrypted string, err error) {
	result, err := attemptScript.Run(ctx, s.client,
		[]string{key, contactAttemptsKey(contactID)},
		s.settings.AttemptsPerCode, s.settings.ContactAttemptsPerHour, int(contactWindow/time.Second),
	).Slice()
	if err != nil {
		return 0, "", "", fmt.Errorf("count the attempt: %w", err)
	}
	attempt, _ = result[0].(int64)
	switch {
	case attempt == 0:
		return 0, "", "", ErrInvalidCode
	case attempt < 0:
		return 0, "", "", ErrTooManyAttempts
	case len(result) != 3:
		return 0, "", "", fmt.Errorf("unexpected attempt result with %d values", len(result))
	}
	storedMAC, _ = result[1].(string)
	encrypted, _ = result[2].(string)
	return attempt, storedMAC, encrypted, nil
}

// encryptContact encrypts contact with AES-GCM. The session ID is
// authenticated data, so a ciphertext cannot be moved to another session.
func (s *Store) encryptContact(sessionID, contact string) (string, error) {
	nonce := make([]byte, s.contacts.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	sealed := s.contacts.Seal(nonce, nonce, []byte(contact), []byte(sessionID))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Store) decryptContact(sessionID, encrypted string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(sealed) < s.contacts.NonceSize() {
		return "", errors.New("decrypt the contact: malformed ciphertext")
	}
	nonce, ciphertext := sealed[:s.contacts.NonceSize()], sealed[s.contacts.NonceSize():]
	contact, err := s.contacts.Open(nil, nonce, ciphertext, []byte(sessionID))
	if err != nil {
		// Usually a changed secret: sessions created before the change
		// cannot be read anymore.
		return "", fmt.Errorf("decrypt the contact: %w", err)
	}
	return string(contact), nil
}
