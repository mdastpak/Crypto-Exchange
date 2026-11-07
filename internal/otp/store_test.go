package otp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeTestRedisDB is the database of this package.
const storeTestRedisDB = redistest.OTP

const testSecret = "an-otp-test-secret-of-at-least-32-bytes"

// newTestRedis returns a client for an empty test database, and skips the
// test when Redis is not reachable.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: storeTestRedisDB})
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		t.Skipf("Redis is not reachable at localhost:6379: %v", err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	t.Cleanup(func() {
		client.FlushDB(context.Background())
		client.Close()
	})
	return client
}

func newTestStore(t *testing.T, settings Settings) (*Store, *redis.Client) {
	t.Helper()
	client := newTestRedis(t)
	if settings.Secret == "" {
		settings.Secret = testSecret
	}
	return NewStore(client, settings), client
}

func issue(t *testing.T, store *Store, contact string) Issued {
	t.Helper()
	issued, err := store.Issue(context.Background(), contact, "")
	require.NoError(t, err)
	return issued
}

// wrongCode returns a code of the same length that differs from code.
func wrongCode(code string) string {
	if code[0] == '0' {
		return "1" + code[1:]
	}
	return "0" + code[1:]
}

func TestStore_VerifiesACodeOnce(t *testing.T) {
	store, client := newTestStore(t, Settings{})
	issued := issue(t, store, "alice@example.com")

	contact, err := store.Verify(context.Background(), issued.SessionID, issued.Code)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", contact)

	_, err = store.Verify(context.Background(), issued.SessionID, issued.Code)
	assert.ErrorIs(t, err, ErrInvalidCode, "a code signs in only once")
	exists, err := client.Exists(context.Background(), store.sessionKey(issued.SessionID)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "a used session is deleted")
}

func TestStore_UnknownOrExpiredSessions(t *testing.T) {
	store, client := newTestStore(t, Settings{})

	_, err := store.Verify(context.Background(), "5b0c8f0e-58a6-4c3b-9d8e-2f7a1c3e9b10", "123456")
	assert.ErrorIs(t, err, ErrInvalidCode)

	issued := issue(t, store, "alice@example.com")
	require.NoError(t, client.Del(context.Background(), store.sessionKey(issued.SessionID)).Err())
	_, err = store.Verify(context.Background(), issued.SessionID, issued.Code)
	assert.ErrorIs(t, err, ErrInvalidCode, "an expired session behaves like an unknown one")
}

func TestStore_LocksASessionAfterItsAttempts(t *testing.T) {
	store, client := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 100})
	issued := issue(t, store, "alice@example.com")

	for i := 0; i < 3; i++ {
		_, err := store.Verify(context.Background(), issued.SessionID, wrongCode(issued.Code))
		require.ErrorIs(t, err, ErrInvalidCode, "attempt %d", i+1)
	}

	fields, err := client.HGetAll(context.Background(), store.sessionKey(issued.SessionID)).Result()
	require.NoError(t, err)
	assert.NotContains(t, fields, fieldCode, "the code hash is erased by the last failed attempt")
	assert.NotContains(t, fields, fieldContact)

	_, err = store.Verify(context.Background(), issued.SessionID, issued.Code)
	assert.ErrorIs(t, err, ErrTooManyAttempts, "the right code is refused once the attempts are used up")
}

// TestStore_AttemptsAreReservedAtomically checks the reservation on its own:
// the per-code limit must hold in Redis even when no failed comparison has
// erased the code yet, which is the situation of concurrent requests.
func TestStore_AttemptsAreReservedAtomically(t *testing.T) {
	store, client := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 5, ContactRequestsPerHour: 100})
	ctx := context.Background()
	issued := issue(t, store, "alice@example.com")
	key := store.sessionKey(issued.SessionID)
	contactID, err := client.HGet(ctx, key, fieldContactID).Result()
	require.NoError(t, err)

	for want := int64(1); want <= 3; want++ {
		attempt, storedMAC, encrypted, err := store.reserveAttempt(ctx, key, contactID)
		require.NoError(t, err)
		assert.Equal(t, want, attempt)
		assert.NotEmpty(t, storedMAC)
		assert.NotEmpty(t, encrypted)
	}
	_, _, _, err = store.reserveAttempt(ctx, key, contactID)
	assert.ErrorIs(t, err, ErrTooManyAttempts, "a fourth attempt for the code must be refused by Redis itself")

	// The contact has 2 of its 5 attempts left after the 3 reserved above.
	second := issue(t, store, "alice@example.com")
	secondKey := store.sessionKey(second.SessionID)
	for i := 0; i < 2; i++ {
		_, _, _, err := store.reserveAttempt(ctx, secondKey, contactID)
		require.NoError(t, err)
	}
	_, _, _, err = store.reserveAttempt(ctx, secondKey, contactID)
	assert.ErrorIs(t, err, ErrTooManyAttempts, "the contact's attempts are reserved atomically as well")
}

func TestStore_ConcurrentAttemptsNeverExceedTheLimit(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 1000})
	issued := issue(t, store, "alice@example.com")

	const parallel = 50
	results := make(chan error, parallel)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Verify(context.Background(), issued.SessionID, wrongCode(issued.Code))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var checked, refused int
	for err := range results {
		switch {
		case errors.Is(err, ErrInvalidCode):
			checked++
		case errors.Is(err, ErrTooManyAttempts):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 3, checked, "only retry_limit guesses may be compared with the code")
	assert.Equal(t, parallel-3, refused)
}

func TestStore_ConcurrentUseOfTheRightCodeSignsInOnce(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 1000})
	issued := issue(t, store, "alice@example.com")

	const parallel = 20
	var successes sync.WaitGroup
	var mu sync.Mutex
	signedIn := 0
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		successes.Add(1)
		go func() {
			defer successes.Done()
			<-start
			if _, err := store.Verify(context.Background(), issued.SessionID, issued.Code); err == nil {
				mu.Lock()
				signedIn++
				mu.Unlock()
			}
		}()
	}
	close(start)
	successes.Wait()

	assert.Equal(t, 1, signedIn)
}

func TestStore_NewCodesDoNotResetTheContactBudget(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 5, ContactRequestsPerHour: 100})
	ctx := context.Background()

	first := issue(t, store, "alice@example.com")
	for i := 0; i < 3; i++ {
		_, err := store.Verify(ctx, first.SessionID, wrongCode(first.Code))
		require.ErrorIs(t, err, ErrInvalidCode)
	}

	// A new code brings a new session, but the contact has 2 attempts left.
	second := issue(t, store, "Alice@Example.com")
	for i := 0; i < 2; i++ {
		_, err := store.Verify(ctx, second.SessionID, wrongCode(second.Code))
		require.ErrorIs(t, err, ErrInvalidCode)
	}

	_, err := store.Verify(ctx, second.SessionID, second.Code)
	assert.ErrorIs(t, err, ErrTooManyAttempts, "the contact's attempts are used up across its codes")
	_, err = store.Issue(ctx, "alice@example.com", "")
	assert.ErrorIs(t, err, ErrTooManyAttempts, "no code is issued that could not be verified")

	other := issue(t, store, "bob@example.com")
	_, err = store.Verify(ctx, other.SessionID, other.Code)
	assert.NoError(t, err, "other contacts are not affected")
}

func TestStore_SignInResetsTheContactBudget(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 4, ContactRequestsPerHour: 100})
	ctx := context.Background()

	first := issue(t, store, "alice@example.com")
	for i := 0; i < 2; i++ {
		_, err := store.Verify(ctx, first.SessionID, wrongCode(first.Code))
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err := store.Verify(ctx, first.SessionID, first.Code)
	require.NoError(t, err)

	second := issue(t, store, "alice@example.com")
	for i := 0; i < 3; i++ {
		_, err := store.Verify(ctx, second.SessionID, wrongCode(second.Code))
		require.ErrorIs(t, err, ErrInvalidCode, "the earlier failures were forgiven by the sign-in")
	}
}

func TestStore_LimitsCodesPerContact(t *testing.T) {
	store, _ := newTestStore(t, Settings{ContactRequestsPerHour: 2})
	ctx := context.Background()

	issue(t, store, "alice@example.com")
	issue(t, store, "ALICE@example.com")
	_, err := store.Issue(ctx, "alice@example.com", "")
	assert.ErrorIs(t, err, ErrTooManyRequests, "email addresses are counted case-insensitively")

	issue(t, store, "09121234567")
}

func TestStore_RedisHoldsNeitherCodesNorContacts(t *testing.T) {
	store, client := newTestStore(t, Settings{CodeLength: 8, TTL: 2 * time.Minute})
	ctx := context.Background()
	const contact = "private.person@example.com"
	issued := issue(t, store, contact)
	_, err := store.Verify(ctx, issued.SessionID, wrongCode(issued.Code))
	require.ErrorIs(t, err, ErrInvalidCode)

	keys, err := client.Keys(ctx, "*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 4, "the session, the contact's total requests, and the requests and attempts of its client")
	for _, key := range keys {
		assert.NotContains(t, key, issued.Code)
		assert.NotContains(t, strings.ToLower(key), "private.person")

		var values []string
		switch typ, err := client.Type(ctx, key).Result(); {
		case err != nil:
			t.Fatal(err)
		case typ == "hash":
			fields, err := client.HGetAll(ctx, key).Result()
			require.NoError(t, err)
			for _, v := range fields {
				values = append(values, v)
			}
		default:
			v, err := client.Get(ctx, key).Result()
			require.NoError(t, err)
			values = append(values, v)
		}
		for _, v := range values {
			assert.NotContains(t, v, issued.Code, "key %s", key)
			assert.NotContains(t, strings.ToLower(v), "private.person", "key %s", key)
		}

		ttl, err := client.TTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, time.Duration(0), "key %s must expire", key)
		assert.LessOrEqual(t, ttl, contactWindow, "key %s", key)
	}
	ttl, err := client.TTL(ctx, store.sessionKey(issued.SessionID)).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, 2*time.Minute, "a session lives as long as its code")
}

func TestStore_ContactsAreEncryptedWithTheSecret(t *testing.T) {
	store, client := newTestStore(t, Settings{})
	ctx := context.Background()
	issued := issue(t, store, "alice@example.com")
	encrypted, err := client.HGet(ctx, store.sessionKey(issued.SessionID), fieldContact).Result()
	require.NoError(t, err)

	contact, err := store.decryptContact(issued.SessionID, encrypted)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", contact)

	other := NewStore(client, Settings{Secret: strings.Repeat("x", MinSecretLength)})
	_, err = other.decryptContact(issued.SessionID, encrypted)
	assert.Error(t, err, "the session ID, which clients know, must not be enough to decrypt the contact")

	_, err = store.decryptContact("5b0c8f0e-58a6-4c3b-9d8e-2f7a1c3e9b10", encrypted)
	assert.Error(t, err, "a ciphertext cannot be moved to another session")
}

func TestStore_CodesAreBoundToTheSecret(t *testing.T) {
	store, client := newTestStore(t, Settings{})
	issued := issue(t, store, "alice@example.com")

	other := NewStore(client, Settings{Secret: strings.Repeat("x", MinSecretLength)})
	_, err := other.Verify(context.Background(), issued.SessionID, issued.Code)

	assert.ErrorIs(t, err, ErrInvalidCode, "a code hash cannot be checked without the secret")
}

func TestStore_NamespacesAreIndependent(t *testing.T) {
	users, client := newTestStore(t, Settings{ContactRequestsPerHour: 1})
	admins := NewStore(client, Settings{Secret: testSecret, Namespace: "admin", ContactRequestsPerHour: 1})
	ctx := context.Background()

	userCode := issue(t, users, "alice@example.com")
	adminCode := issue(t, admins, "alice@example.com")

	_, err := admins.Verify(ctx, userCode.SessionID, userCode.Code)
	assert.ErrorIs(t, err, ErrInvalidCode, "a user sign-in code must not sign in an administrator")
	_, err = users.Verify(ctx, adminCode.SessionID, adminCode.Code)
	assert.ErrorIs(t, err, ErrInvalidCode, "an admin sign-in code must not sign in a user")

	keys, err := client.Keys(ctx, "otp:admin:*").Result()
	require.NoError(t, err)
	assert.NotEmpty(t, keys, "admin keys live under their own prefix")

	_, err = admins.Verify(ctx, adminCode.SessionID, adminCode.Code)
	assert.NoError(t, err)
}

// TestVerify_AcceptsPersianDigits checks #197: a code typed with Persian or
// Arabic-Indic digits, or with surrounding spaces, is the same code.
func TestVerify_AcceptsPersianDigits(t *testing.T) {
	store, _ := newTestStore(t, Settings{})
	for _, typed := range []func(string) string{
		func(code string) string { return mapDigits(code, '۰') },
		func(code string) string { return mapDigits(code, '٠') },
		func(code string) string { return "  " + code + "\n" },
	} {
		issued := issue(t, store, "digits@example.com")
		contact, err := store.Verify(context.Background(), issued.SessionID, typed(issued.Code))
		require.NoError(t, err)
		assert.Equal(t, "digits@example.com", contact)
	}
}

// TestVerify_MalformedCodesSpendNoAttempt checks that a code that cannot be
// right is refused before an attempt is counted.
func TestVerify_MalformedCodesSpendNoAttempt(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 1, ContactAttemptsPerHour: 1})
	issued := issue(t, store, "careful@example.com")
	for _, malformed := range []string{"", "12345", "1234567", "abcdef", "12 456"} {
		_, err := store.Verify(context.Background(), issued.SessionID, malformed)
		assert.ErrorIs(t, err, ErrInvalidCode, "%q", malformed)
	}
	contact, err := store.Verify(context.Background(), issued.SessionID, issued.Code)
	require.NoError(t, err, "the only attempt is still there")
	assert.Equal(t, "careful@example.com", contact)
}

func mapDigits(s string, zero rune) string {
	out := []rune{}
	for _, r := range s {
		out = append(out, zero+(r-'0'))
	}
	return string(out)
}

// issueFrom issues a code for contact requested by client.
func issueFrom(t *testing.T, store *Store, contact, client string) Issued {
	t.Helper()
	issued, err := store.Issue(context.Background(), contact, client)
	require.NoError(t, err)
	return issued
}

// TestStore_StrangerCannotLockOutTheOwner checks #187: someone who knows only
// a contact requests codes for it and fails them, which uses up the attempts
// of the stranger's client, not those of the owner.
func TestStore_StrangerCannotLockOutTheOwner(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 3})
	ctx := context.Background()
	const victim = "victim@example.com"

	attack := issueFrom(t, store, victim, "198.51.100.66")
	for i := 0; i < 3; i++ {
		_, err := store.Verify(ctx, attack.SessionID, wrongCode(attack.Code))
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err := store.Issue(ctx, victim, "198.51.100.66")
	require.ErrorIs(t, err, ErrTooManyAttempts, "the stranger is locked out")

	own := issueFrom(t, store, victim, "203.0.113.7")
	contact, err := store.Verify(ctx, own.SessionID, own.Code)
	require.NoError(t, err, "the owner still signs in")
	assert.Equal(t, victim, contact)
}

// TestStore_AttemptsCountAgainstTheRequester checks that guessing on a code
// that another client requested still counts for the requester, so that a
// stranger gains nothing by sending guesses from many addresses.
func TestStore_AttemptsCountAgainstTheRequester(t *testing.T) {
	store, _ := newTestStore(t, Settings{AttemptsPerCode: 2, ContactAttemptsPerHour: 2})
	ctx := context.Background()
	a := issueFrom(t, store, "target@example.com", "client-a")
	for i := 0; i < 2; i++ {
		_, err := store.Verify(ctx, a.SessionID, wrongCode(a.Code))
		require.ErrorIs(t, err, ErrInvalidCode)
	}
	_, err := store.Issue(ctx, "target@example.com", "client-a")
	assert.ErrorIs(t, err, ErrTooManyAttempts)
	_, err = store.Issue(ctx, "target@example.com", "client-b")
	assert.NoError(t, err)
}

func TestStore_LimitsRequestsPerClient(t *testing.T) {
	store, _ := newTestStore(t, Settings{ContactRequestsPerHour: 2, ContactRequestsTotalPerHour: 10})
	ctx := context.Background()
	issueFrom(t, store, "busy@example.com", "client-a")
	issueFrom(t, store, "busy@example.com", "client-a")
	_, err := store.Issue(ctx, "Busy@Example.com", "client-a")
	assert.ErrorIs(t, err, ErrTooManyRequests)
	issueFrom(t, store, "busy@example.com", "client-b")
}

// TestStore_TotalLimitSparesKnownClients checks the total limit, which
// bounds the codes sent to a contact from every client together: once it is
// reached, a new client is refused, but a client that signed in with the
// contact before is not.
func TestStore_TotalLimitSparesKnownClients(t *testing.T) {
	store, _ := newTestStore(t, Settings{ContactRequestsPerHour: 2, ContactRequestsTotalPerHour: 3})
	ctx := context.Background()
	const owner = "owner@example.com"

	first := issueFrom(t, store, owner, "home")
	_, err := store.Verify(ctx, first.SessionID, first.Code)
	require.NoError(t, err, "home becomes a known client")

	issueFrom(t, store, owner, "stranger-1")
	issueFrom(t, store, owner, "stranger-2")
	_, err = store.Issue(ctx, owner, "stranger-3")
	assert.ErrorIs(t, err, ErrTooManyRequests, "the total is reached")

	issueFrom(t, store, owner, "home")
}

func TestSettings_TotalRequestsAtLeastPerClient(t *testing.T) {
	err := Settings{Secret: strings.Repeat("s", 32), ContactRequestsPerHour: 30, ContactRequestsTotalPerHour: 10}.Validate(false)
	assert.ErrorContains(t, err, "contact_requests_total_per_hour")
	assert.Equal(t, 30, Settings{ContactRequestsPerHour: 30}.WithDefaults().ContactRequestsTotalPerHour, "the default is never below the per-client limit")
	assert.Equal(t, DefaultContactRequestsTotalPerHour, Settings{}.WithDefaults().ContactRequestsTotalPerHour)
}
