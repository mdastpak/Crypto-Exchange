package agreements

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestCovers(t *testing.T) {
	none := config.LegalConfig{}
	assert.True(t, Covers(none, nil), "without documents nothing is asked")
	assert.Empty(t, Required(none))

	legal := config.LegalConfig{Documents: []config.LegalDocument{{Kind: "terms", Version: "2"}, {Kind: "privacy", Version: "1"}}}
	assert.False(t, Covers(legal, nil))
	assert.False(t, Covers(legal, map[string]string{"terms": "2"}), "every document is needed")
	assert.False(t, Covers(legal, map[string]string{"terms": "1", "privacy": "1"}), "an old version is not the current one")
	assert.True(t, Covers(legal, map[string]string{"terms": "2", "privacy": "1", "other": "9"}))
	assert.True(t, Covers(legal, map[string]string{"terms": " 2 ", "privacy": "1"}))
}

func TestSignUpBlocked(t *testing.T) {
	legal := config.LegalConfig{}
	legal.SignUp.BlockedCountries = []string{"KP", "CU"}
	assert.True(t, SignUpBlocked(legal, "KP"))
	assert.True(t, SignUpBlocked(legal, "cu"))
	assert.False(t, SignUpBlocked(legal, "DE"))
	assert.False(t, SignUpBlocked(legal, ""), "a country that is not known is not refused")
}
