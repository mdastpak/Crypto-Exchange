package routing

import (
	"context"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/venues"
	"github.com/mdastpak/crypto-exchange/internal/venues/state"
)

// The reasons of StateVenues, beside those of the venue state (#385).
const (
	ReasonNotMapped     = "not_mapped"
	ReasonVenueDisabled = "venue_disabled"
)

// StateVenues reads the venues from their state (#385): the snapshots of
// their markets, balances, and networks, which every node reads the same.
type StateVenues struct {
	State *state.Store
	// Mappings are the asset mappings of the enabled venues, by name; nil
	// reads them from Config.
	Mappings map[string]venues.Mapping
	// Config gives the fees and the slippage of each venue.
	Config func() *config.Configuration
}

func (v StateVenues) mapping(name string) (venues.Mapping, bool) {
	if v.Mappings != nil {
		m, ok := v.Mappings[name]
		return m, ok
	}
	if v.Config == nil {
		return nil, false
	}
	m, ok := state.MappingsOf(v.Config().Venues)[name]
	return m, ok
}

func (v StateVenues) venue(name string) (config.VenueConfig, bool) {
	if v.Config == nil {
		return config.VenueConfig{}, false
	}
	c, ok := v.Config().Venues.Exchanges[name]
	return c, ok && c.Enabled
}

// Market implements Venues.
func (v StateVenues) Market(ctx context.Context, name, base, quote string) (VenueMarket, error) {
	c, enabled := v.venue(name)
	mapping, mapped := v.mapping(name)
	if !enabled || !mapped {
		return VenueMarket{Reasons: []string{ReasonVenueDisabled}}, nil
	}
	m := VenueMarket{FeeBPS: c.FeeBPS, SlippageBPS: c.SlippageBPS}
	vb, _, okB := mapping.Venue(base, "")
	vq, _, okQ := mapping.Venue(quote, "")
	if !okB || !okQ {
		m.Reasons = []string{ReasonNotMapped}
		return m, nil
	}
	in, ok, err := v.State.InstrumentOf(ctx, name, vb, vq)
	if err != nil {
		return m, err
	}
	if !ok {
		m.Reasons = []string{state.ReasonMarketUnknown}
		return m, nil
	}
	m.Symbol = in.Symbol
	m.Lot, m.MinQuantity, m.MinNotional = in.Lot.Decimal(), in.MinQuantity.Decimal(), in.MinNotional.Decimal()
	r, err := v.State.MarketRoutable(ctx, name, in.Symbol)
	if err != nil {
		return m, err
	}
	m.Reasons = r.Reasons
	return m, nil
}

// Funds implements Venues.
func (v StateVenues) Funds(ctx context.Context, name, currency, network string) (Funds, error) {
	mapping, mapped := v.mapping(name)
	if _, enabled := v.venue(name); !enabled || !mapped {
		return Funds{Reasons: []string{ReasonVenueDisabled}}, nil
	}
	asset, venueNetwork, ok := mapping.Venue(currency, network)
	if !ok {
		return Funds{Reasons: []string{ReasonNotMapped}}, nil
	}
	r, err := v.State.Routable(ctx, name, asset, venueNetwork)
	if err != nil {
		return Funds{}, err
	}
	f := Funds{Reasons: r.Reasons}
	available, err := v.State.Available(ctx, name, asset)
	if err != nil {
		return Funds{}, err
	}
	f.Available = available.Decimal()
	if venueNetwork != "" {
		fee, err := v.State.WithdrawFee(ctx, name, asset, venueNetwork)
		if err != nil {
			return Funds{}, err
		}
		f.WithdrawFee = fee.Decimal()
	}
	return f, nil
}
