// Package assetregistry loads and checks the asset registry file
// (config/assets.yaml): the assets the exchange supports, the networks each
// one moves on, and the markets. An asset is defined once by its symbol, with
// one entry per network, so that USDT on TRON and on Ethereum is one asset
// and one balance.
//
// Loading refuses the whole file on any problem, and reports every problem at
// once: a repeated symbol, network, or market, an unknown type, status, or
// network, a contract address that fails its network's format or checksum,
// limits that contradict each other, or a market that names an unknown
// asset. A wrong contract address would send funds to an arbitrary contract.
package assetregistry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"go.yaml.in/yaml/v3"
)

// Registry is the content of the registry file.
type Registry struct {
	Assets  []Asset  `yaml:"assets"`
	Markets []Market `yaml:"markets"`
}

// Asset is one asset, with the networks it moves on.
type Asset struct {
	Symbol      string             `yaml:"symbol"`
	Name        string             `yaml:"name"`
	Type        shared.AssetType   `yaml:"type"`
	Status      shared.AssetStatus `yaml:"status"`
	Decimals    int                `yaml:"decimals"`
	Description string             `yaml:"description"`
	LogoURL     string             `yaml:"logo_url"`
	Website     string             `yaml:"website"`
	Networks    []Network          `yaml:"networks"`
}

// Network is how an asset moves on one network.
type Network struct {
	Network         string `yaml:"network"`
	ContractAddress string `yaml:"contract_address"`
	// FeeAsset is the asset that pays the network fee, such as TRX for a
	// TRC20 token.
	FeeAsset          string                `yaml:"fee_asset"`
	MinWithdrawal     string                `yaml:"min_withdrawal"`
	MaxWithdrawal     string                `yaml:"max_withdrawal"`
	WithdrawalFee     string                `yaml:"withdrawal_fee"`
	DepositEnabled    bool                  `yaml:"deposit_enabled"`
	WithdrawalEnabled bool                  `yaml:"withdrawal_enabled"`
	CustodyMode       shared.SettlementMode `yaml:"custody_mode"`
	RequiresTag       bool                  `yaml:"requires_tag"`
	ExplorerURL       string                `yaml:"explorer_url"`
}

// Market is a pair of asset symbols with its trading rules.
type Market struct {
	Base         string              `yaml:"base"`
	Quote        string              `yaml:"quote"`
	Status       market.MarketStatus `yaml:"status"`
	MinOrderSize string              `yaml:"min_order_size"`
	MaxOrderSize string              `yaml:"max_order_size"`
	MinNotional  string              `yaml:"min_notional"`
	TickSize     string              `yaml:"tick_size"`
	LotSize      string              `yaml:"lot_size"`
	MakerFeeBPS  int                 `yaml:"maker_fee_bps"`
	TakerFeeBPS  int                 `yaml:"taker_fee_bps"`
}

// maxDecimals is the most decimals an amount holds: exact_decimal refuses
// a value of more (#314, docs/architecture.md#amounts-prices-and-ratios).
const maxDecimals = 36

// maxFeeBPS bounds a fee at 10%; a larger fee is a typo.
const maxFeeBPS = 1000

// networks are the known networks, with their native asset and whether
// assets on them have contracts.
var networks = map[string]struct {
	native    string
	contracts bool
	validate  func(string) error
}{
	"internal": {},
	"bitcoin":  {native: "BTC"},
	"ethereum": {native: "ETH", contracts: true, validate: ValidateEthereumAddress},
	"tron":     {native: "TRX", contracts: true, validate: ValidateTronAddress},
}

var (
	assetTypes    = []shared.AssetType{shared.AssetTypeFiat, shared.AssetTypeCoin, shared.AssetTypeToken, shared.AssetTypeStable, shared.AssetTypeWrapped}
	assetStatuses = []shared.AssetStatus{shared.AssetStatusActive, shared.AssetStatusInactive, shared.AssetStatusDeprecated, shared.AssetStatusDelisted}
	custodyModes  = []shared.SettlementMode{shared.SettlementModeOffChain, shared.SettlementModeOnChain}
	marketStatus  = []market.MarketStatus{market.MarketStatusActive, market.MarketStatusInactive, market.MarketStatusMaintenance, market.MarketStatusSuspended, market.MarketStatusDelisted}
)

// Load reads and checks the registry file at path, which comes from the
// configuration (assets.registry_path).
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	registry, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return registry, nil
}

// Parse decodes and checks a registry. A field that the registry does not
// know is an error, so that a typo cannot silently drop a setting.
func Parse(data []byte) (*Registry, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var registry Registry
	if err := decoder.Decode(&registry); err != nil {
		return nil, fmt.Errorf("decoding the asset registry: %w", err)
	}
	if err := registry.Validate(); err != nil {
		return nil, err
	}
	return &registry, nil
}

// Validate reports every problem of the registry, one per line.
func (r *Registry) Validate() error {
	var problems []string
	add := func(format string, args ...interface{}) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	symbols := make(map[string]bool)
	contracts := make(map[string]string)
	for i, a := range r.Assets {
		where := fmt.Sprintf("asset %d (%s)", i+1, a.Symbol)
		if a.Symbol == "" || strings.ToUpper(a.Symbol) != a.Symbol || strings.ContainsAny(a.Symbol, " _/") {
			add("%s: symbol must be upper case, without spaces, underscores, or slashes; a network goes in networks", where)
		}
		if symbols[a.Symbol] {
			add("%s: symbol %s is defined twice; define one asset with a network entry per network", where, a.Symbol)
		}
		symbols[a.Symbol] = true
		if a.Name == "" {
			add("%s: name is required", where)
		}
		if !contains(assetTypes, a.Type) {
			add("%s: unknown type %q", where, a.Type)
		}
		if !contains(assetStatuses, a.Status) {
			add("%s: unknown status %q", where, a.Status)
		}
		if a.Decimals < 0 || a.Decimals > maxDecimals {
			add("%s: decimals is %d; it must be from 0 to %d", where, a.Decimals, maxDecimals)
		}
		if len(a.Networks) == 0 {
			add("%s: at least one network is required", where)
		}

		seen := make(map[string]bool)
		for _, n := range a.Networks {
			at := fmt.Sprintf("%s on %s", where, n.Network)
			info, known := networks[n.Network]
			if !known {
				add("%s: unknown network %q", at, n.Network)
				continue
			}
			if seen[n.Network] {
				add("%s: network %s is defined twice", at, n.Network)
			}
			seen[n.Network] = true
			if (a.Type == shared.AssetTypeFiat) != (n.Network == "internal") {
				add("%s: fiat assets, and only they, use the internal network", at)
			}
			switch {
			case info.contracts && a.Symbol != info.native && n.ContractAddress == "":
				add("%s: a token needs its contract_address", at)
			case (!info.contracts || a.Symbol == info.native) && n.ContractAddress != "":
				add("%s: the native asset of a network, or an asset on a network without contracts, has no contract_address", at)
			case n.ContractAddress != "":
				if err := info.validate(n.ContractAddress); err != nil {
					add("%s: contract_address %s: %v", at, n.ContractAddress, err)
				} else if other, dup := contracts[n.Network+"/"+strings.ToLower(n.ContractAddress)]; dup {
					add("%s: contract_address is also the contract of %s", at, other)
				} else {
					contracts[n.Network+"/"+strings.ToLower(n.ContractAddress)] = a.Symbol
				}
			}
			if !contains(custodyModes, n.CustodyMode) || (n.Network == "internal") != (n.CustodyMode == shared.SettlementModeOffChain) {
				add("%s: custody_mode %q must be OFF_CHAIN on the internal network and ON_CHAIN elsewhere", at, n.CustodyMode)
			}
			if info.native != "" && n.FeeAsset != info.native {
				add("%s: fee_asset is %q; network fees on %s are paid in %s", at, n.FeeAsset, n.Network, info.native)
			}
			problems = append(problems, limitProblems(at, "withdrawal", n.MinWithdrawal, n.MaxWithdrawal, n.WithdrawalFee)...)
		}
	}

	pairs := make(map[string]bool)
	for i, m := range r.Markets {
		where := fmt.Sprintf("market %d (%s/%s)", i+1, m.Base, m.Quote)
		if !symbols[m.Base] || !symbols[m.Quote] {
			add("%s: base and quote must be assets of the registry", where)
		}
		if m.Base == m.Quote {
			add("%s: base and quote must differ", where)
		}
		if pairs[m.Base+"/"+m.Quote] {
			add("%s: the market is defined twice", where)
		}
		pairs[m.Base+"/"+m.Quote] = true
		if !contains(marketStatus, m.Status) {
			add("%s: unknown status %q", where, m.Status)
		}
		problems = append(problems, limitProblems(where, "order size", m.MinOrderSize, m.MaxOrderSize, "0")...)
		for name, value := range map[string]string{"min_notional": m.MinNotional, "tick_size": m.TickSize, "lot_size": m.LotSize} {
			d, err := shared.NewDecimalFromString(value)
			switch {
			case err != nil:
				add("%s: %s %q is not a decimal", where, name, value)
			case name != "min_notional" && !d.IsPositive():
				add("%s: %s must be positive", where, name)
			case d.IsNegative():
				add("%s: %s must not be negative", where, name)
			}
		}
		for name, bps := range map[string]int{"maker_fee_bps": m.MakerFeeBPS, "taker_fee_bps": m.TakerFeeBPS} {
			if bps < 0 || bps > maxFeeBPS {
				add("%s: %s is %d; it must be from 0 to %d", where, name, bps, maxFeeBPS)
			}
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// limitProblems checks a minimum, a maximum (0 means none), and a fee.
func limitProblems(where, what, minimum, maximum, fee string) []string {
	var problems []string
	parse := func(name, value string) (shared.Decimal, bool) {
		d, err := shared.NewDecimalFromString(value)
		if err != nil || d.IsNegative() {
			problems = append(problems, fmt.Sprintf("%s: %s %q must be a decimal of at least 0", where, name, value))
			return d, false
		}
		return d, true
	}
	lo, okLo := parse("minimum "+what, minimum)
	hi, okHi := parse("maximum "+what, maximum)
	parse(what+" fee", fee)
	if okLo && okHi && hi.IsPositive() && lo.GreaterThan(hi) {
		problems = append(problems, fmt.Sprintf("%s: the minimum %s %s exceeds the maximum %s", where, what, lo, hi))
	}
	return problems
}

func contains[T comparable](list []T, value T) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
