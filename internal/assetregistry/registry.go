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
//
// The file is the import format of the currencies (#309): an asset is a
// currency, and currencies.Store.Import, which `admin assets import` runs,
// loads the file into the currencies, their networks, and the markets.
package assetregistry

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	Symbol string `yaml:"symbol"`
	// Name is the English name, and FaName the Persian one, of the
	// currency that the asset is in the exchange (#309).
	Name        string             `yaml:"name"`
	FaName      string             `yaml:"fa_name"`
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
	FeeAsset string `yaml:"fee_asset"`
	// MinDeposit is the smallest deposit that is credited, "0" when left
	// out.
	MinDeposit        string                `yaml:"min_deposit"`
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
		if a.FaName == "" {
			add("%s: fa_name is required", where)
		}
		if !contains(assetTypes, a.Type) {
			add("%s: unknown type %q", where, a.Type)
		}
		if !contains(assetStatuses, a.Status) {
			add("%s: unknown status %q", where, a.Status)
		}
		for _, p := range CheckDecimals(a.Decimals) {
			add("%s: %s", where, p.Text)
		}
		if len(a.Networks) == 0 {
			add("%s: at least one network is required", where)
		}

		seen := make(map[string]bool)
		for _, n := range a.Networks {
			at := fmt.Sprintf("%s on %s", where, n.Network)
			found := CheckNetwork(a.Symbol, a.Type == shared.AssetTypeFiat, n)
			for _, p := range found {
				add("%s: %s", at, p.Text)
			}
			if _, known := networks[n.Network]; !known {
				continue
			}
			if seen[n.Network] {
				add("%s: network %s is defined twice", at, n.Network)
			}
			seen[n.Network] = true
			// A contract is one asset's on its network.
			if n.ContractAddress == "" || slices.ContainsFunc(found, func(p Problem) bool { return p.Field == "contract_address" }) {
				continue
			}
			key := n.Network + "/" + strings.ToLower(n.ContractAddress)
			if other, dup := contracts[key]; dup {
				add("%s: contract_address is also the contract of %s", at, other)
			} else {
				contracts[key] = a.Symbol
			}
		}
	}

	pairs := make(map[string]bool)
	for i, m := range r.Markets {
		where := fmt.Sprintf("market %d (%s/%s)", i+1, m.Base, m.Quote)
		if !symbols[m.Base] || !symbols[m.Quote] {
			add("%s: base and quote must be assets of the registry", where)
		}
		if pairs[m.Base+"/"+m.Quote] {
			add("%s: the market is defined twice", where)
		}
		pairs[m.Base+"/"+m.Quote] = true
		for _, p := range CheckMarket(m) {
			add("%s: %s", where, p.Text)
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// Problem is a problem of one entry, with the field it is about, so that
// the admin API answers it by field (#57).
type Problem struct {
	Field, Text string
}

// CheckDecimals checks the decimals of an amount on a network.
func CheckDecimals(decimals int) []Problem {
	if decimals < 0 || decimals > maxDecimals {
		return []Problem{{"decimals", fmt.Sprintf("decimals is %d; it must be from 0 to %d", decimals, maxDecimals)}}
	}
	return nil
}

// CheckNetwork checks a network of the asset symbol, which is fiat or not,
// as the registry file and the admin API take it: a known network, the
// contract that its format and checksum accept, none for the native asset,
// the custody mode, the currency of the network fee, and the limits. That a
// contract is no other asset's is checked by the caller, which knows them.
func CheckNetwork(symbol string, fiat bool, n Network) []Problem {
	var problems []Problem
	add := func(field, format string, args ...interface{}) {
		problems = append(problems, Problem{field, fmt.Sprintf(format, args...)})
	}
	info, known := networks[n.Network]
	if !known {
		add("network", "unknown network %q", n.Network)
		return problems
	}
	if fiat != (n.Network == "internal") {
		add("network", "fiat assets, and only they, use the internal network")
	}
	switch {
	case info.contracts && symbol != info.native && n.ContractAddress == "":
		add("contract_address", "a token needs its contract_address")
	case (!info.contracts || symbol == info.native) && n.ContractAddress != "":
		add("contract_address", "the native asset of a network, or an asset on a network without contracts, has no contract_address")
	case n.ContractAddress != "":
		if err := info.validate(n.ContractAddress); err != nil {
			add("contract_address", "contract_address %s: %v", n.ContractAddress, err)
		}
	}
	if !contains(custodyModes, n.CustodyMode) || (n.Network == "internal") != (n.CustodyMode == shared.SettlementModeOffChain) {
		add("custody_mode", "custody_mode %q must be OFF_CHAIN on the internal network and ON_CHAIN elsewhere", n.CustodyMode)
	}
	if info.native != "" && n.FeeAsset != info.native {
		add("fee_asset", "fee_asset is %q; network fees on %s are paid in %s", n.FeeAsset, n.Network, info.native)
	}
	problems = append(problems, limitProblems("withdrawal", "min_withdrawal", n.MinWithdrawal, "max_withdrawal", n.MaxWithdrawal)...)
	problems = append(problems, amountProblems("withdrawal_fee", "withdrawal fee", n.WithdrawalFee)...)
	if d, err := shared.NewDecimalFromString(n.MinDepositOrZero()); err != nil || d.IsNegative() {
		add("min_deposit", "min_deposit %q must be a decimal of at least 0", n.MinDeposit)
	}
	return problems
}

// CheckMarket checks the rules of a market: two currencies, a known status,
// the order sizes, a positive tick and lot, and fees of at most 10%. That
// its currencies exist is checked by the caller.
func CheckMarket(m Market) []Problem {
	var problems []Problem
	add := func(field, format string, args ...interface{}) {
		problems = append(problems, Problem{field, fmt.Sprintf(format, args...)})
	}
	if m.Base == m.Quote {
		add("quote", "base and quote must differ")
	}
	if !contains(marketStatus, m.Status) {
		add("status", "unknown status %q", m.Status)
	}
	problems = append(problems, limitProblems("order size", "min_order_size", m.MinOrderSize, "max_order_size", m.MaxOrderSize)...)
	for _, f := range []struct{ name, value string }{{"min_notional", m.MinNotional}, {"tick_size", m.TickSize}, {"lot_size", m.LotSize}} {
		d, err := shared.NewDecimalFromString(f.value)
		switch {
		case err != nil:
			add(f.name, "%s %q is not a decimal", f.name, f.value)
		case f.name != "min_notional" && !d.IsPositive():
			add(f.name, "%s must be positive", f.name)
		case d.IsNegative():
			add(f.name, "%s must not be negative", f.name)
		}
	}
	for _, f := range []struct {
		name string
		bps  int
	}{{"maker_fee_bps", m.MakerFeeBPS}, {"taker_fee_bps", m.TakerFeeBPS}} {
		if f.bps < 0 || f.bps > maxFeeBPS {
			add(f.name, "%s is %d; it must be from 0 to %d", f.name, f.bps, maxFeeBPS)
		}
	}
	return problems
}

// limitProblems checks a minimum and a maximum, of which 0 means none.
func limitProblems(what, minField, minimum, maxField, maximum string) []Problem {
	lo, problems := amount(minField, "minimum "+what, minimum)
	hi, more := amount(maxField, "maximum "+what, maximum)
	problems = append(problems, more...)
	if problems == nil && hi.IsPositive() && lo.GreaterThan(hi) {
		problems = append(problems, Problem{minField, fmt.Sprintf("the minimum %s %s exceeds the maximum %s", what, lo, hi)})
	}
	return problems
}

// amountProblems checks an amount of at least 0.
func amountProblems(field, name, value string) []Problem {
	_, problems := amount(field, name, value)
	return problems
}

func amount(field, name, value string) (shared.Decimal, []Problem) {
	d, err := shared.NewDecimalFromString(value)
	if err != nil || d.IsNegative() {
		return d, []Problem{{field, fmt.Sprintf("%s %q must be a decimal of at least 0", name, value)}}
	}
	return d, nil
}

func contains[T comparable](list []T, value T) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// MinDepositOrZero is MinDeposit, or "0" when it is left out.
func (n Network) MinDepositOrZero() string {
	if n.MinDeposit == "" {
		return "0"
	}
	return n.MinDeposit
}
