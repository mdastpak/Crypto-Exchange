package venues

import (
	"fmt"
	"sort"
	"strings"
)

// AssetMapping says which of our currencies an asset of a venue is, and
// which of our networks (#57) each of its networks is.
type AssetMapping struct {
	// Currency is our symbol of the currency, such as USDT.
	Currency string `yaml:"currency"`
	// Networks maps the venue's name of each network, such as TRX, to ours,
	// such as tron. A network that is not here is not routable.
	Networks map[string]string `yaml:"networks"`
	// ReturnNetwork is our network on which the asset comes back to our
	// wallet (#510); empty is the cheapest of Networks.
	ReturnNetwork string `yaml:"return_network"`
}

// ReturnNetworks returns the venue's names of the networks on which the
// asset may come back to our wallet: that of ReturnNetwork, or else all
// of Networks, sorted.
func (a AssetMapping) ReturnNetworks() []string {
	var out []string
	for vn, n := range a.Networks {
		if a.ReturnNetwork == "" || n == a.ReturnNetwork {
			out = append(out, vn)
		}
	}
	sort.Strings(out)
	return out
}

// Mapping maps the assets of one venue, by the venue's symbol, to ours. It
// is configured, never guessed from a symbol: two venues may call one
// token by different names, or two tokens by one.
type Mapping map[string]AssetMapping

// Ours returns our currency and network of a venue's asset and network;
// ok is false for one that is not mapped, which is not routable. An empty
// network asks for the currency only.
func (m Mapping) Ours(asset, network string) (currency, ourNetwork string, ok bool) {
	a, ok := m[asset]
	if !ok {
		return "", "", false
	}
	if network == "" {
		return a.Currency, "", true
	}
	n, ok := a.Networks[network]
	if !ok {
		return "", "", false
	}
	return a.Currency, n, true
}

// Venue returns the venue's asset and network of our currency and network;
// ok is false when the venue maps none.
func (m Mapping) Venue(currency, ourNetwork string) (asset, network string, ok bool) {
	for venueAsset, a := range m {
		if !strings.EqualFold(a.Currency, currency) {
			continue
		}
		if ourNetwork == "" {
			return venueAsset, "", true
		}
		for vn, n := range a.Networks {
			if n == ourNetwork {
				return venueAsset, vn, true
			}
		}
	}
	return "", "", false
}

// Check refuses a mapping that maps two assets to one currency, or two
// networks of an asset to one network, so that Venue has one answer.
func (m Mapping) Check() error {
	byCurrency := map[string]string{}
	for asset, a := range m {
		if a.Currency == "" {
			return fmt.Errorf("the asset %s maps to no currency", asset)
		}
		key := strings.ToUpper(a.Currency)
		if other, ok := byCurrency[key]; ok {
			return fmt.Errorf("the assets %s and %s both map to %s", min(asset, other), max(asset, other), a.Currency)
		}
		byCurrency[key] = asset
		byNetwork := map[string]string{}
		for vn, n := range a.Networks {
			if n == "" {
				return fmt.Errorf("the network %s of %s maps to no network", vn, asset)
			}
			if other, ok := byNetwork[n]; ok {
				return fmt.Errorf("the networks %s and %s of %s both map to %s", min(vn, other), max(vn, other), asset, n)
			}
			byNetwork[n] = vn
		}
	}
	return nil
}
