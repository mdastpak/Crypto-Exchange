package stub

import (
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/sourcetest"
)

func TestStubs_KeepTheContracts(t *testing.T) {
	sourcetest.CheckCatalog(t, NewCatalog("coingecko"))
	venue := NewVenue("stub_global", marketdata.RegionGlobal)
	sourcetest.CheckVenue(t, venue)
	sourcetest.CheckCandles(t, venue, "BTCUSDT", source.Interval1h, time.Now())
	sourcetest.CheckCandles(t, venue, "ETHUSDT", source.Interval1d, time.Now())
}
