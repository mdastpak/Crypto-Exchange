package totp

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rfcSecret is the SHA-1 seed of the RFC 6238 test vectors.
var rfcSecret = []byte("12345678901234567890")

// TestCode_RFC6238Vectors checks the 8-digit SHA-1 test vectors of RFC 6238,
// Appendix B. The 6-digit codes are the last 6 digits of the same values.
func TestCode_RFC6238Vectors(t *testing.T) {
	vectors := map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	}
	for unix, want := range vectors {
		step := Step(time.Unix(unix, 0))
		assert.Equal(t, want, code(rfcSecret, step, 8), "time %d", unix)
		assert.Equal(t, want[2:], Code(rfcSecret, step), "time %d", unix)
	}
}

func TestVerify_AcceptsOneStepOfSkew(t *testing.T) {
	now := time.Unix(1234567890, 0)
	current := Step(now)

	for _, step := range []int64{current - 1, current, current + 1} {
		matched, ok := Verify(rfcSecret, Code(rfcSecret, step), now)
		assert.True(t, ok, "step %d", step-current)
		assert.Equal(t, step, matched, "Verify must report the matched step")
	}
	for _, step := range []int64{current - 2, current + 2} {
		_, ok := Verify(rfcSecret, Code(rfcSecret, step), now)
		assert.False(t, ok, "step %d must be outside the window", step-current)
	}
}

func TestVerify_RejectsMalformedCodes(t *testing.T) {
	now := time.Unix(1234567890, 0)
	valid := Code(rfcSecret, Step(now))

	for _, code := range []string{"", "12345", "1234567", valid + " ", " " + valid[1:]} {
		_, ok := Verify(rfcSecret, code, now)
		assert.False(t, ok, "%q", code)
	}
	_, ok := Verify([]byte("another secret of 20b"), valid, now)
	assert.False(t, ok, "a code belongs to its secret")
}

func TestGenerateSecret(t *testing.T) {
	first, err := GenerateSecret()
	require.NoError(t, err)
	second, err := GenerateSecret()
	require.NoError(t, err)

	assert.Len(t, first, SecretSize)
	assert.NotEqual(t, first, second)
	assert.Len(t, EncodeSecret(first), 32, "20 bytes are 32 base32 characters without padding")
}

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI("EncryptedDB Exchange", "admin@example.com", rfcSecret)

	parsed, err := url.Parse(uri)
	require.NoError(t, err)
	assert.Equal(t, "otpauth", parsed.Scheme)
	assert.Equal(t, "totp", parsed.Host)
	assert.Equal(t, "/EncryptedDB Exchange:admin@example.com", parsed.Path)
	query := parsed.Query()
	assert.Equal(t, EncodeSecret(rfcSecret), query.Get("secret"))
	assert.Equal(t, "EncryptedDB Exchange", query.Get("issuer"))
	assert.Equal(t, "SHA1", query.Get("algorithm"))
	assert.Equal(t, "6", query.Get("digits"))
	assert.Equal(t, "30", query.Get("period"))
	assert.False(t, strings.Contains(uri, "+"), "spaces must be encoded as %%20 for authenticator apps")
}
