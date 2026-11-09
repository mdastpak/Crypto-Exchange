package admin

import (
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/stretchr/testify/assert"
)

// TestMessageKeyFamilies checks the keys that the messages of this package
// build from a value (#290).
func TestMessageKeyFamilies(t *testing.T) {
	for _, field := range []string{"symbol", "contract"} {
		assert.True(t, i18n.HasKey(i18n.Key("CURRENCY_FIELD_"+strings.ToUpper(field))), "field %s", field)
	}
	for action, message := range controlFailures {
		assert.True(t, i18n.HasKey(message.Key), "control action %s", action)
	}
	for action, message := range currencyWriteFailures {
		assert.True(t, i18n.HasKey(message.Key), "currency action %s", action)
	}
}
