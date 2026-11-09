package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/region"
)

// The features of one market (#272). Each declares the regions it belongs
// to and how the configuration switches it on; a release whose
// deployment.region is not one of them refuses to start while it is on,
// with a message that names the setting, so that a misconfigured release
// fails at once instead of half working. A new Iran-only feature, such as
// a payment gateway or a KYC provider, adds itself here when it is added
// (CONTRIBUTING.md).

// IranianSources are the market data sources of the Iranian exchanges,
// whose venues have the region ir. The tests of the exchange sources check
// that the two lists agree.
var IranianSources = []string{"nobitex", "wallex"}

// regionFeature is a feature of one market.
type regionFeature struct {
	// what names the feature for the message.
	what    string
	regions []region.Region
	// on returns the settings that switch the feature on, each with what
	// to do instead, or none when it is off.
	on func(c *Configuration) []regionSetting
	// worker is a feature of cmd/market-worker, which the worker checks
	// too.
	worker bool
}

// regionSetting is a setting that switches a feature on.
type regionSetting struct {
	key, value, instead string
}

var regionFeatures = []regionFeature{
	{
		what:    "the Shahkar check of Iranian mobile numbers (#253)",
		regions: []region.Region{region.Iran},
		on: func(c *Configuration) []regionSetting {
			if p := c.Identity.MobileOwnership.Provider; p != "" && p != MobileOwnershipNone {
				return []regionSetting{{"identity.mobile_ownership.provider", p, "set it to none"}}
			}
			return nil
		},
	},
	{
		what:    "Kavenegar, an Iranian SMS gateway",
		regions: []region.Region{region.Iran},
		on: func(c *Configuration) []regionSetting {
			if p := c.Notifications.SMS.Provider; p == "kavenegar" {
				return []regionSetting{{"notifications.sms.provider", p, "leave it empty, which turns SMS off"}}
			}
			return nil
		},
	},
	{
		what:    "the Iranian exchanges of the market data catalog",
		regions: []region.Region{region.Iran},
		worker:  true,
		on: func(c *Configuration) []regionSetting {
			var out []regionSetting
			for _, name := range slices.Sorted(maps.Keys(c.MarketData.Sources)) {
				if c.MarketData.Sources[name].Enabled && slices.Contains(IranianSources, name) {
					out = append(out, regionSetting{"market_data.sources." + name + ".enabled", "true", "set it to false"})
				}
			}
			return out
		},
	},
	{
		what:    "the Iranian exchanges in the market data API",
		regions: []region.Region{region.Iran},
		on: func(c *Configuration) []regionSetting {
			if slices.Contains(c.MarketData.API.Regions, "ir") {
				return []regionSetting{{"market_data.api.regions", strings.Join(c.MarketData.API.Regions, ","), "set it to [global]"}}
			}
			return nil
		},
	},
	{
		what:    "toman (IRT) prices in the market data catalog",
		regions: []region.Region{region.Iran},
		worker:  true,
		on: func(c *Configuration) []regionSetting {
			if slices.Contains(c.MarketData.Candles.Quotes, "IRT") {
				return []regionSetting{{"market_data.candles.quotes", strings.Join(c.MarketData.Candles.Quotes, ","), "leave IRT out"}}
			}
			return nil
		},
	},
	{
		what:    "a site for visitors from Iran",
		regions: []region.Region{region.Iran},
		on: func(c *Configuration) []regionSetting {
			var out []regionSetting
			for i, s := range c.Sites.List {
				if slices.ContainsFunc(s.Countries, func(country string) bool { return strings.EqualFold(country, "IR") }) {
					out = append(out, regionSetting{fmt.Sprintf("sites.list[%d].countries", i), strings.Join(s.Countries, ","), "leave IR out"})
				}
			}
			return out
		},
	},
}

// regionProblems reports each feature that is on outside its regions; the
// worker checks only the features of the worker.
func (c *Configuration) regionProblems(p *problems, worker bool) {
	r := c.Region()
	for _, f := range regionFeatures {
		if (worker && !f.worker) || r.In(f.regions...) {
			continue
		}
		for _, s := range f.on(c) {
			p.add(s.key, "is %q, which turns on %s, a feature of the %s region only; deployment.region (%s) is %s: %s, or set the region to %s",
				s.value, f.what, regionNames(f.regions), envVarFor("deployment.region"), r, s.instead, regionNames(f.regions))
		}
	}
}

func regionNames(regions []region.Region) string {
	names := make([]string, len(regions))
	for i, r := range regions {
		names[i] = r.String()
	}
	return strings.Join(names, " or ")
}
