package passwordhash

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests hash with little memory, so that they are fast.
func TestMain(m *testing.M) {
	current = argonParams{memory: 64, time: 1, threads: 1}
	os.Exit(m.Run())
}

func TestHashPassword_VerifiesOnlyItsPassword(t *testing.T) {
	ctx := context.Background()
	hash, err := Argon2id{}.Hash(ctx, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=64,t=1,p=1$"), hash)
	assert.NotContains(t, hash, "correct")

	match, outdated, err := Argon2id{}.Verify(ctx, hash, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, match)
	assert.False(t, outdated)

	match, _, err = Argon2id{}.Verify(ctx, hash, "correct horse batterz")
	require.NoError(t, err)
	assert.False(t, match)
	again, err := Argon2id{}.Hash(ctx, "correct horse battery")
	require.NoError(t, err)
	assert.NotEqual(t, hash, again, "each hash has its own salt")
}

func TestVerifyPassword_ReportsAHashWithOtherSettings(t *testing.T) {
	ctx := context.Background()
	hash, err := Argon2id{}.Hash(ctx, "correct horse battery")
	require.NoError(t, err)

	saved := current
	defer func() { current = saved }()
	current = argonParams{memory: 128, time: 1, threads: 1}
	match, outdated, err := Argon2id{}.Verify(ctx, hash, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, match, "the hash carries its own settings")
	assert.True(t, outdated)
}

func TestVerifyPassword_RefusesAMalformedOrCostlyHash(t *testing.T) {
	ctx := context.Background()
	for _, hash := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=16$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=64,t=0,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=64,t=1,p=0$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=4294967295,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=64,t=4000000,p=1$c2FsdHNhbHQ$aGFzaGhhc2g",
		"$argon2id$v=19$m=64,t=1,p=1$!!!$aGFzaGhhc2g",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$",
	} {
		_, _, err := Argon2id{}.Verify(ctx, hash, "correct horse battery")
		assert.ErrorIs(t, err, ErrMalformedHash, hash)
	}
}

func TestHashPassword_StopsWaitingWhenTheRequestEnds(t *testing.T) {
	for range cap(hashSlots) {
		require.NoError(t, acquire(context.Background()))
	}
	defer func() {
		for range cap(hashSlots) {
			release()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Argon2id{}.Hash(ctx, "correct horse battery")
	assert.ErrorIs(t, err, context.Canceled)
}
