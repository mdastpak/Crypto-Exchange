package otc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/keys"

	"github.com/redis/go-redis/v9"
)

// Quote is a firm quote: the user may take it at Price until ExpiresAt.
type Quote struct {
	ID string `json:"id" format:"uuid"`
	// UserHK and SubAccountID are the account that asked for it; only that
	// account may take it.
	UserHK       string  `json:"-"`
	SubAccountID *string `json:"sub_account_id,omitempty" format:"uuid"`
	Market       string  `json:"market" example:"BTC/USDT"`
	Side         Side    `json:"side" enums:"buy,sell" example:"buy"`
	// Quantity is in the base currency, and Price, Total, and Fee in the
	// quote currency. Total is Quantity × Price. A buy pays Total + Fee,
	// and a sell receives Total − Fee.
	Quantity shared.Decimal `json:"quantity" swaggertype:"string" example:"0.5"`
	Price    shared.Decimal `json:"price" swaggertype:"string" example:"65000.12"`
	Total    shared.Decimal `json:"total" swaggertype:"string" example:"32500.06"`
	Fee      shared.Decimal `json:"fee" swaggertype:"string" example:"65"`
	FeeBPS   int            `json:"fee_bps" example:"20"`
	// FeeCurrency is the quote currency.
	FeeCurrency string    `json:"fee_currency" example:"USDT"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Components are the parts of the price, which the execution records
	// (#62); they are not shown to the user.
	Components Components `json:"-"`
}

// stored is a quote as Redis holds it, with its parts.
type stored struct {
	Quote
	UserHK     string     `json:"user_hk"`
	Components Components `json:"components"`
}

func (q Quote) owner() string { return owner(q.UserHK, q.SubAccountID) }

func owner(userHK string, subAccount *string) string {
	if subAccount == nil {
		return userHK
	}
	return userHK + "/" + *subAccount
}

// The refusals of a take.
var (
	// ErrQuoteNotFound is a quote that does not exist: never issued,
	// expired, taken already, or of another account. Who asked for a
	// quote is not told to anyone else.
	ErrQuoteNotFound = errors.New("otc: no such quote")
	// ErrQuoteExpired is a quote taken after its expiry, which Redis had not
	// removed yet.
	ErrQuoteExpired = errors.New("otc: the quote expired")
	// ErrQuoteTampered is a quote whose signature does not match: it was
	// changed in Redis. It is never executed.
	ErrQuoteTampered = errors.New("otc: the quote was changed after it was issued")
)

// DataKeys are the data-protection keys (keys.Live) that sign the quotes.
type DataKeys interface {
	Current(purpose keys.Purpose) (keys.Key, error)
	Version(ctx context.Context, purpose keys.Purpose, version int) (keys.Key, error)
}

// Store keeps the quotes in Redis, each under its own key with a TTL of
// its expiry, signed with an HMAC so that a quote changed in Redis is
// refused. Every instance reads the quotes of the others: the signing key
// is derived from the otp-mac data key, which they share.
type Store struct {
	redis redis.UniversalClient
	keys  DataKeys
	// process signs the quotes without data keys, which only this
	// instance can then take: a development setup with one instance.
	process []byte

	mu      sync.Mutex
	derived map[int][]byte
}

// KeyPrefix is the prefix of the Redis keys of the quotes.
const KeyPrefix = "otc:quote:"

// macLabel derives the signing key of the quotes from an otp-mac key, so
// that a signature of a quote is never one of anything else.
const macLabel = "otc-quote/v1"

// NewStore returns a store of the quotes in client, signed with the
// otp-mac keys of dataKeys; nil signs them with a key of this process.
func NewStore(client redis.UniversalClient, dataKeys DataKeys) (*Store, error) {
	s := &Store{redis: client, derived: map[int][]byte{}}
	if dataKeys != nil {
		s.keys = dataKeys
		return s, nil
	}
	s.process = make([]byte, 32)
	if _, err := rand.Read(s.process); err != nil {
		return nil, fmt.Errorf("otc: a process key: %w", err)
	}
	return s, nil
}

// signer returns the key that signs a new quote, and its version; 0 is
// the key of the process.
func (s *Store) signer() ([]byte, int, error) {
	if s.keys == nil {
		return s.process, 0, nil
	}
	key, err := s.keys.Current(keys.PurposeOTPMAC)
	if err != nil {
		return nil, 0, fmt.Errorf("otc: the otp-mac data key: %w", err)
	}
	k, err := s.derive(key)
	return k, key.Version(), err
}

// verifier returns the key of a stored version.
func (s *Store) verifier(ctx context.Context, version int) ([]byte, error) {
	if s.keys == nil {
		if version != 0 {
			return nil, fmt.Errorf("%w: it names a data key, and this instance has none", ErrQuoteTampered)
		}
		return s.process, nil
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: it names no data key, and this instance signs with data keys only", ErrQuoteTampered)
	}
	key, err := s.keys.Version(ctx, keys.PurposeOTPMAC, version)
	if err != nil {
		return nil, fmt.Errorf("otc: the otp-mac data key: %w", err)
	}
	return s.derive(key)
}

func (s *Store) derive(key keys.Key) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.derived[key.Version()]; ok {
		return k, nil
	}
	mac, err := key.NewMAC()
	if err != nil {
		return nil, err
	}
	mac.Write([]byte(macLabel))
	k := mac.Sum(nil)
	s.derived[key.Version()] = k
	return k, nil
}

// sign returns the HMAC of a quote: its ID, its account, and its body, so
// that none of them can be moved to another.
func sign(key []byte, id, owner string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	writeField(mac, id)
	writeField(mac, owner)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func writeField(h hash.Hash, s string) {
	h.Write([]byte(strconv.Itoa(len(s))))
	h.Write([]byte{':'})
	h.Write([]byte(s))
}

// Save stores q until it expires.
func (s *Store) Save(ctx context.Context, q Quote) error {
	ttl := time.Until(q.ExpiresAt)
	if ttl <= 0 {
		return ErrQuoteExpired
	}
	body, err := json.Marshal(stored{Quote: q, UserHK: q.UserHK, Components: q.Components})
	if err != nil {
		return fmt.Errorf("otc: encoding the quote: %w", err)
	}
	key, version, err := s.signer()
	if err != nil {
		return err
	}
	o := q.owner()
	_, err = s.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, KeyPrefix+q.ID, "owner", o, "body", body, "mac", sign(key, q.ID, o, body), "v", version)
		p.PExpire(ctx, KeyPrefix+q.ID, ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("otc: saving the quote: %w", err)
	}
	return nil
}

// takeScript removes a quote of the account ARGV[1] and returns it. A
// quote of another account stays, so that nobody else can spend it: it
// answers {1}. A missing one answers {0}.
var takeScript = redis.NewScript(`
local owner = redis.call('HGET', KEYS[1], 'owner')
if not owner then return {0} end
if owner ~= ARGV[1] then return {1} end
local v = redis.call('HMGET', KEYS[1], 'body', 'mac', 'v')
redis.call('DEL', KEYS[1])
return {2, v[1], v[2], v[3]}
`)

// Take removes the quote id of the account and returns it, once: a second
// take finds nothing. It refuses a quote of another account, which stays,
// one that expired, and one whose signature does not match, with
// ErrQuoteNotFound, ErrQuoteExpired, and ErrQuoteTampered. The execution
// (#62) takes a quote before it trades, so that it trades at most once.
func (s *Store) Take(ctx context.Context, id, userHK string, subAccount *string, now time.Time) (Quote, error) {
	o := owner(userHK, subAccount)
	res, err := takeScript.Run(ctx, s.redis, []string{KeyPrefix + id}, o).Slice()
	if err != nil {
		return Quote{}, fmt.Errorf("otc: taking the quote: %w", err)
	}
	if len(res) == 0 {
		return Quote{}, ErrQuoteNotFound
	}
	if status, _ := res[0].(int64); status != 2 || len(res) != 4 {
		return Quote{}, ErrQuoteNotFound
	}
	body, _ := res[1].(string)
	mac, _ := res[2].(string)
	version, err := strconv.Atoi(fmt.Sprint(res[3]))
	if err != nil {
		return Quote{}, ErrQuoteTampered
	}
	key, err := s.verifier(ctx, version)
	if err != nil {
		return Quote{}, err
	}
	if !hmac.Equal([]byte(mac), []byte(sign(key, id, o, []byte(body)))) {
		return Quote{}, ErrQuoteTampered
	}
	var st stored
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		return Quote{}, ErrQuoteTampered
	}
	q := st.Quote
	q.UserHK, q.Components = st.UserHK, st.Components
	if q.ID != id || q.owner() != o {
		return Quote{}, ErrQuoteTampered
	}
	if !now.Before(q.ExpiresAt) {
		return Quote{}, ErrQuoteExpired
	}
	return q, nil
}
