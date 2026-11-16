package stub_test

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/stub"
	"github.com/mdastpak/crypto-exchange/internal/venues/venuetest"
)

func newStub(t *testing.T) venuetest.Harness {
	v := stub.New("stub")
	v.SetBalance("USDT", shared.MustNewDecimalFromString("100000"))
	v.SetBalance("BTC", shared.MustNewDecimalFromString("0"))
	v.AddInstrument(venues.Instrument{Symbol: "BTCUSDT", Base: "BTC", Quote: "USDT", Trading: true},
		shared.MustNewDecimalFromString("60000"))
	return venuetest.Harness{Venue: v, Symbol: "BTCUSDT", Quantity: shared.MustNewDecimalFromString("0.01"),
		LimitPrice: shared.MustNewDecimalFromString("50000"), LoseNextAnswer: v.LoseNextAnswer}
}

func TestStub_PassesTheContract(t *testing.T) {
	venuetest.Run(t, newStub)
}
