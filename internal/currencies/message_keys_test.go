package currencies

import (
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/stretchr/testify/assert"
)

// TestCapabilities_HaveMessageKeys checks that a refusal names every
// capability in the catalogs (#290).
func TestCapabilities_HaveMessageKeys(t *testing.T) {
	for _, k := range Capabilities {
		assert.True(t, i18n.HasKey(i18n.Key("CAPABILITY_"+strings.ToUpper(string(k)))), "no message key for capability %s", k)
	}
}
