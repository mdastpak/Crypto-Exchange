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

	"github.com/mdastpak/crypto-exchange/internal/contact"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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
	// the current hour, from the client or in total.
	ErrTooManyRequests = errors.New("too many codes requested for this contact")
)

// contactWindow is the period of the per-contact limits. It starts with the
// first request or attempt and is not extended by later ones.
const contactWindow = time.Hour

// knownClientTTL is how long a client that signed in with a contact stays
// known to it. A known client may still request codes after the contact's
// total limit is reached, so that strangers cannot lock out its owner (#187).
const knownClientTTL = 30 * 24 * time.Hour

// Redis layout. Keys never contain a code or a raw contact. A store with a
// namespace prefixes its keys with "otp:<namespace>:" instead of "otp:".
const (
	fieldCode      = "code"       // HMAC of the session ID and the code
	fieldContact   = "contact"    // the contact, encrypted
	fieldContactID = "contact_id" // HMAC of the normalized contact
	fieldClientID  = "client_id"  // HMAC of the client that requested the code
	fieldAttempts  = "attempts"
)

// Store keeps sign-in sessions in Redis. A session holds an HMAC of its code,
// never the code itself, and the contact encrypted with a key derived from the
// secret, so reading Redis reveals neither.
type Store struct {
	client    *redis.Client
	settings  Settings
	keyPrefix string
	macKey    []byte
	contacts  cipher.AEAD
}

// NewStore returns a store with the given settings. Without a secret it uses
// a random one, which only works while a single process issues and verifies
// the codes; Settings.Validate refuses that in release mode.
func NewStore(client *redis.Client, settings Settings) *Store {
	settings = settings.WithDefaults()
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
	keyPrefix := "otp:"
	if settings.Namespace != "" {
		keyPrefix += settings.Namespace + ":"
	}
	return &Store{
		client:    client,
		settings:  settings,
		keyPrefix: keyPrefix,
		macKey:    deriveKey(secret, "otp code and contact hashing"),
		contacts:  contacts,
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

func (s *Store) sessionKey(sessionID string) string { return s.keyPrefix + "session:" + sessionID }

// clientID identifies a client in Redis keys, without its address.
func (s *Store) clientID(client string) string {
	return hex.EncodeToString(s.mac("client", client))
}

// The per-contact limits (#187). The requests and the attempts are counted
// per contact and client, so that a stranger who fails codes for a contact,
// or requests many, uses up only the budget of the stranger's own client.
// The total limit bounds the codes that one contact receives from every
// client together, and the cost of the SMS sent to it; a client that has
// signed in with the contact before is exempt from it.

func (s *Store) contactTotalKey(contactID string) string {
	return s.keyPrefix + "contact:" + contactID + ":requests"
}

func (s *Store) contactRequestsKey(contactID, clientID string) string {
	return s.keyPrefix + "contact:" + contactID + ":client:" + clientID + ":requests"
}

func (s *Store) contactAttemptsKey(contactID, clientID string) string {
	return s.keyPrefix + "contact:" + contactID + ":client:" + clientID + ":attempts"
}

func (s *Store) knownClientKey(contactID, clientID string) string {
	return s.keyPrefix + "contact:" + contactID + ":client:" + clientID + ":known"
}

// Issued is a new sign-in session.
type Issued struct {
	SessionID string
	Code      string
}

// issueScript counts a code request for a contact from a client. It refuses
// the request when the client has used up its attempts for the contact, so
// that no code is sent that could not be verified; when the client has
// requested too many codes for the contact; or when the contact has
// received too many codes in total, unless the client is known to it.
//
// KEYS[1]: requests of the client, KEYS[2]: attempts of the client,
// KEYS[3]: total requests of the contact, KEYS[4]: the known-client marker
// ARGV[1]: requests per hour per client, ARGV[2]: attempts per hour per
// client, ARGV[3]: window in seconds, ARGV[4]: total requests per hour
// Returns 1 when allowed, -1 when there are too many requests, and -2 when
// there are too many attempts.
var issueScript = redis.NewScript(`
local attempts = tonumber(redis.call('GET', KEYS[2]) or '0')
if attempts >= tonumber(ARGV[2]) then
	return -2
end
local known = redis.call('EXISTS', KEYS[4]) == 1
local total = tonumber(redis.call('GET', KEYS[3]) or '0')
if not known and total >= tonumber(ARGV[4]) then
	return -1
end
local requests = redis.call('INCR', KEYS[1])
if requests == 1 then
	redis.call('EXPIRE', KEYS[1], ARGV[3])
end
if requests > tonumber(ARGV[1]) then
	return -1
end
if redis.call('INCR', KEYS[3]) == 1 then
	redis.call('EXPIRE', KEYS[3], ARGV[3])
end
return 1
`)

// Issue creates a session with a new code for contact, requested by client.
// client identifies the requester for the per-contact limits, usually its
// address as clientip.Key groups it; every instance must pass the same
// identity for the same client. An empty client is one client of its own.
func (s *Store) Issue(ctx context.Context, contact, client string) (Issued, error) {
	contactID, clientID := s.contactID(contact), s.clientID(client)
	result, err := issueScript.Run(ctx, s.client,
		[]string{
			s.contactRequestsKey(contactID, clientID), s.contactAttemptsKey(contactID, clientID),
			s.contactTotalKey(contactID), s.knownClientKey(contactID, clientID),
		},
		s.settings.ContactRequestsPerHour, s.settings.ContactAttemptsPerHour, int(contactWindow/time.Second),
		s.settings.ContactRequestsTotalPerHour,
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

	key := s.sessionKey(sessionID)
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key,
			fieldCode, hex.EncodeToString(s.mac("code", sessionID, code)),
			fieldContact, encrypted,
			fieldContactID, contactID,
			fieldClientID, clientID,
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
// contact and the client that requested the code, and returns what is needed to check the code. A session whose code
// field is gone is locked: its attempts are used up.
//
// KEYS[1]: session, KEYS[2]: attempts of the contact and the client
// ARGV[1]: attempts per code, ARGV[2]: attempts per contact and client,
// ARGV[3]: window in seconds
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
// code ends the session, so it works only once, resets the attempts of the
// contact from the client that requested the code, and makes that client
// known to the contact. The attempts count against the client that
// requested the code, not the one that sends it: a stranger who requests
// codes for someone else's contact and fails them locks only the stranger's
// own client out (#187). The attempt is counted before the code is checked, so
// concurrent attempts can never exceed the limits.
//
// The code may be typed with Persian or Arabic-Indic digits and surrounding
// white space (#197). A code that is not then CodeLength digits cannot be
// right, so it is refused without spending an attempt.
func (s *Store) Verify(ctx context.Context, sessionID, code string) (string, error) {
	code, wellFormed := contact.Code(code, s.settings.CodeLength)
	if !wellFormed {
		return "", ErrInvalidCode
	}
	key := s.sessionKey(sessionID)
	ids, err := s.client.HMGet(ctx, key, fieldContactID, fieldClientID).Result()
	if err != nil {
		return "", fmt.Errorf("load the session: %w", err)
	}
	contactID, _ := ids[0].(string)
	clientID, _ := ids[1].(string)
	if contactID == "" {
		return "", ErrInvalidCode
	}
	if clientID == "" {
		// A session issued before #187 counted its attempts per contact
		// only; it expires within otp.auth.ttl_seconds of the upgrade.
		clientID = s.clientID("")
	}

	attempt, storedMAC, encrypted, err := s.reserveAttempt(ctx, key, s.contactAttemptsKey(contactID, clientID))
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
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, s.contactAttemptsKey(contactID, clientID))
		pipe.Set(ctx, s.knownClientKey(contactID, clientID), 1, knownClientTTL)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reset the contact attempts: %w", err)
	}
	return s.decryptContact(sessionID, encrypted)
}

// reserveAttempt counts one attempt for the session and in attemptsKey, the
// attempts of its contact and client, and returns the attempt number with the
// stored code HMAC and encrypted contact.
func (s *Store) reserveAttempt(ctx context.Context, key, attemptsKey string) (attempt int64, storedMAC, encrypted string, err error) {
	result, err := attemptScript.Run(ctx, s.client,
		[]string{key, attemptsKey},
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
