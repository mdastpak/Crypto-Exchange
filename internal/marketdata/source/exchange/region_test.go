package exchange

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIranianVenues_AreRefusedByAnInternationalRelease keeps the venues of
// the ir region and the sources that an international release refuses (#272)
// the same list.
func TestIranianVenues_AreRefusedByAnInternationalRelease(t *testing.T) {
	var iranian []string
	for _, name := range Names {
		src, err := New(name, nil)
		require.NoError(t, err)
		if src.Venue().Region == marketdata.RegionIran {
			iranian = append(iranian, name)
		}
	}
	assert.ElementsMatch(t, config.IranianSources, iranian)
}
