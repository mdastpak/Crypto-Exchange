package user

import (
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/favorites"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/stretchr/testify/assert"
)

// TestMessageKeyFamilies checks that every value that a message of this
// package names by a key built from it has that key (#290).
func TestMessageKeyFamilies(t *testing.T) {
	for _, kind := range []favorites.Kind{favorites.KindCurrency, favorites.KindPair} {
		upper := strings.ToUpper(string(kind))
		assert.True(t, i18n.HasKey(i18n.Key("FAVORITE_KIND_"+upper)), "kind %s", kind)
		assert.True(t, i18n.HasKey(i18n.Key("FAVORITE_NOT_LISTED_"+upper)), "kind %s", kind)
	}
	for _, typ := range []string{"email", "mobile"} {
		assert.True(t, i18n.HasKey(i18n.Key("CHANNEL_"+strings.ToUpper(typ))), "channel %s", typ)
	}
}
