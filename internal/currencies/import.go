package currencies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/models"
)

// importLockID is the advisory lock that runs one import at a time.
const importLockID int64 = 0x309_0001

// ImportReport is what an import did, or, in a dry run, would do.
type ImportReport struct {
	// Created are the symbols of the currencies that the import created,
	// and Existing those it found and left as they are: the admin API
	// changes a currency.
	Created, Existing []string
	Networks, Markets ImportCounts
	// Notes are differences that the import does not change, such as the
	// names of a currency that exists.
	Notes []string
}

// ImportCounts counts the rows of networks or markets that an import
// created, changed, and found as the file has them.
type ImportCounts struct{ Created, Updated, Unchanged int }

// ErrImport refuses a file that cannot be applied, such as a new currency
// with a status that only a delisting reaches.
var ErrImport = errors.New("the registry file cannot be imported")

// Import applies the registry file reg to the currencies (#309), in one
// transaction and under a lock, with an audit event from origin for each
// change:
//
//   - an asset whose symbol no currency has becomes a new currency, from
//     its names, type, logo, and status (ACTIVE is approved, INACTIVE
//     pending); a currency that exists is left as it is;
//   - each network of an asset is written to currency_networks, with the
//     decimals of the asset and the currency of its fee;
//   - each market is written to markets, by its base and quote currencies.
//
// Networks and markets that the file leaves out are kept: custody and the
// orders may reference them. With dryRun, the changes are made and rolled
// back, and the report says what they would be.
func (s Store) Import(ctx context.Context, reg *assetregistry.Registry, origin audit.Event, dryRun bool) (ImportReport, error) {
	if err := reg.Validate(); err != nil {
		return ImportReport{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return ImportReport{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, importLockID); err != nil {
		return ImportReport{}, err
	}
	var report ImportReport
	hks, err := importCurrencies(ctx, tx, reg, origin, &report)
	if err != nil {
		return ImportReport{}, err
	}
	for _, a := range reg.Assets {
		for _, n := range a.Networks {
			if err := importNetwork(ctx, tx, hks, a, n, origin, &report); err != nil {
				return ImportReport{}, fmt.Errorf("%s on %s: %w", a.Symbol, n.Network, err)
			}
		}
	}
	for _, m := range reg.Markets {
		if err := importMarket(ctx, tx, hks, m, origin, &report); err != nil {
			return ImportReport{}, fmt.Errorf("market %s/%s: %w", m.Base, m.Quote, err)
		}
	}
	if dryRun {
		return report, nil
	}
	return report, tx.Commit()
}

// importCurrencies finds or creates the currency of each asset, and returns
// their hks by symbol.
func importCurrencies(ctx context.Context, tx *sql.Tx, reg *assetregistry.Registry, origin audit.Event, report *ImportReport) (map[string]string, error) {
	hks := make(map[string]string, len(reg.Assets))
	for _, a := range reg.Assets {
		info := infoOf(a)
		var hk, enName, faName string
		err := tx.QueryRowContext(ctx, `SELECT hk, info->>'en_name', info->>'fa_name' FROM currencies
			WHERE info->>'symbol' = $1 AND deleted_at IS NULL FOR UPDATE`, a.Symbol).Scan(&hk, &enName, &faName)
		if err == nil {
			hks[a.Symbol] = hk
			report.Existing = append(report.Existing, a.Symbol)
			if enName != info.EnName || faName != info.FaName {
				report.Notes = append(report.Notes, fmt.Sprintf("%s: the names differ from the file's; the admin API changes them", a.Symbol))
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		status, err := statusOf(a.Status)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Symbol, err)
		}
		if problems := Validate(status, info); problems != nil {
			return nil, fmt.Errorf("%w: %s: %s", ErrImport, a.Symbol, describe(problems))
		}
		w, err := create(ctx, tx, status, info, origin)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.Symbol, err)
		}
		hks[a.Symbol] = w.HK
		report.Created = append(report.Created, a.Symbol)
	}
	return hks, nil
}

// infoOf is the info of the currency of an asset. A token keeps one
// contract in its info, which the currency API shows: that of its first
// network with one. Custody reads the contracts of currency_networks.
func infoOf(a assetregistry.Asset) models.CurrencyInfo {
	info := models.CurrencyInfo{
		EnName: a.Name, FaName: a.FaName, Symbol: a.Symbol, LogoURL: a.LogoURL,
		IfFiat:  a.Type == shared.AssetTypeFiat,
		IsToken: a.Type == shared.AssetTypeToken || a.Type == shared.AssetTypeStable || a.Type == shared.AssetTypeWrapped,
	}
	for _, n := range a.Networks {
		if n.ContractAddress != "" {
			info.Contract = n.ContractAddress
			break
		}
	}
	return info
}

// statusOf is the status of a new currency of an asset. A deprecated or
// delisted asset is not created: a currency reaches those states through a
// delisting.
func statusOf(s shared.AssetStatus) (string, error) {
	switch s {
	case shared.AssetStatusActive:
		return models.CurrencyStatusApproved, nil
	case shared.AssetStatusInactive:
		return models.CurrencyStatusPending, nil
	}
	return "", fmt.Errorf("%w: a new currency cannot be %s; delist a currency through the delisting routes", ErrImport, s)
}

// describe lists problems by field, in a stable order.
func describe(problems map[string]string) string {
	fields := make([]string, 0, len(problems))
	for field := range problems {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	parts := make([]string, len(fields))
	for i, field := range fields {
		parts[i] = field + " " + problems[field]
	}
	return strings.Join(parts, "; ")
}

// importNetwork writes one network of an asset to currency_networks.
func importNetwork(ctx context.Context, tx *sql.Tx, hks map[string]string, a assetregistry.Asset, n assetregistry.Network, origin audit.Event, report *ImportReport) error {
	hk := hks[a.Symbol]
	feeHK := ""
	if n.FeeAsset != "" {
		var err error
		if feeHK, err = currencyOf(ctx, tx, hks, n.FeeAsset); err != nil {
			return err
		}
	}
	after := map[string]interface{}{
		"network": n.Network, "contract_address": n.ContractAddress, "decimals": a.Decimals,
		"custody_mode": string(n.CustodyMode), "deposit_enabled": n.DepositEnabled, "withdrawal_enabled": n.WithdrawalEnabled,
		"min_deposit": n.MinDepositOrZero(), "min_withdrawal": n.MinWithdrawal, "max_withdrawal": n.MaxWithdrawal,
		"withdrawal_fee": n.WithdrawalFee, "fee_currency_hk": feeHK, "requires_tag": n.RequiresTag, "explorer_url": n.ExplorerURL,
	}
	var stored storedNetwork
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(contract_address, ''), decimals, custody_mode::text, deposit_enabled, withdrawal_enabled,
			min_deposit::text, min_withdrawal::text, max_withdrawal::text, withdrawal_fee::text, COALESCE(fee_currency_hk::text, ''),
			requires_tag, COALESCE(explorer_url, '')
		FROM currency_networks WHERE currency_hk = $1 AND network = $2 FOR UPDATE`, hk, n.Network).
		Scan(&stored.contract, &stored.decimals, &stored.custodyMode, &stored.depositEnabled, &stored.withdrawalEnabled,
			&stored.minDeposit, &stored.minWithdrawal, &stored.maxWithdrawal, &stored.withdrawalFee, &stored.feeHK,
			&stored.requiresTag, &stored.explorerURL)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var changes *audit.Changes
	if found {
		before := stored.fields(n.Network)
		changes = changedValues(before, after)
		if len(changes.After) == 0 {
			report.Networks.Unchanged++
			return nil
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO currency_networks (currency_hk, network, contract_address, decimals, custody_mode,
			deposit_enabled, withdrawal_enabled, min_deposit, min_withdrawal, max_withdrawal, withdrawal_fee, fee_currency_hk,
			requires_tag, explorer_url)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5::settlement_mode, $6, $7, $8::numeric, $9::numeric, $10::numeric, $11::numeric,
			NULLIF($12, '')::uuid, $13, NULLIF($14, ''))
		ON CONFLICT (currency_hk, network) DO UPDATE SET contract_address = EXCLUDED.contract_address, decimals = EXCLUDED.decimals,
			custody_mode = EXCLUDED.custody_mode, deposit_enabled = EXCLUDED.deposit_enabled,
			withdrawal_enabled = EXCLUDED.withdrawal_enabled, min_deposit = EXCLUDED.min_deposit,
			min_withdrawal = EXCLUDED.min_withdrawal, max_withdrawal = EXCLUDED.max_withdrawal,
			withdrawal_fee = EXCLUDED.withdrawal_fee, fee_currency_hk = EXCLUDED.fee_currency_hk,
			requires_tag = EXCLUDED.requires_tag, explorer_url = EXCLUDED.explorer_url`,
		hk, n.Network, n.ContractAddress, a.Decimals, string(n.CustodyMode), n.DepositEnabled, n.WithdrawalEnabled,
		n.MinDepositOrZero(), n.MinWithdrawal, n.MaxWithdrawal, n.WithdrawalFee, feeHK, n.RequiresTag, n.ExplorerURL)
	if err != nil {
		return err
	}
	if found {
		report.Networks.Updated++
	} else {
		report.Networks.Created++
		changes = &audit.Changes{After: after}
	}
	return record(ctx, tx, origin, audit.ActionCurrencyNetworkSet, hk, changes)
}

// storedNetwork is a row of currency_networks as an import compares it.
type storedNetwork struct {
	contract, custodyMode, minDeposit, minWithdrawal, maxWithdrawal, withdrawalFee, feeHK, explorerURL string
	decimals                                                                                           int
	depositEnabled, withdrawalEnabled, requiresTag                                                     bool
}

func (s storedNetwork) fields(network string) map[string]interface{} {
	return map[string]interface{}{
		"network": network, "contract_address": s.contract, "decimals": s.decimals, "custody_mode": s.custodyMode,
		"deposit_enabled": s.depositEnabled, "withdrawal_enabled": s.withdrawalEnabled, "min_deposit": s.minDeposit,
		"min_withdrawal": s.minWithdrawal, "max_withdrawal": s.maxWithdrawal, "withdrawal_fee": s.withdrawalFee,
		"fee_currency_hk": s.feeHK, "requires_tag": s.requiresTag, "explorer_url": s.explorerURL,
	}
}

// changedValues keeps the fields that differ between before and after.
// Amounts are compared as numbers, so that "1" and "1.0" are the same.
func changedValues(before, after map[string]interface{}) *audit.Changes {
	changes := &audit.Changes{Before: map[string]interface{}{}, After: map[string]interface{}{}}
	for name, value := range after {
		if sameValue(before[name], value) {
			continue
		}
		changes.Before[name], changes.After[name] = before[name], value
	}
	return changes
}

func sameValue(a, b interface{}) bool {
	as, aString := a.(string)
	bs, bString := b.(string)
	if aString && bString {
		if da, err := shared.NewDecimalFromString(as); err == nil {
			if db, err := shared.NewDecimalFromString(bs); err == nil {
				return da.Equal(db)
			}
		}
	}
	return a == b
}

// currencyOf returns the hk of the currency of symbol: one of the file, or
// one that exists.
func currencyOf(ctx context.Context, tx *sql.Tx, hks map[string]string, symbol string) (string, error) {
	if hk, ok := hks[symbol]; ok {
		return hk, nil
	}
	var hk string
	err := tx.QueryRowContext(ctx, `SELECT hk FROM currencies WHERE info->>'symbol' = $1 AND deleted_at IS NULL`, symbol).Scan(&hk)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no currency %s", ErrImport, symbol)
	}
	return hk, err
}

// importMarket writes one market to markets.
func importMarket(ctx context.Context, tx *sql.Tx, hks map[string]string, m assetregistry.Market, origin audit.Event, report *ImportReport) error {
	base, err := currencyOf(ctx, tx, hks, m.Base)
	if err != nil {
		return err
	}
	quote, err := currencyOf(ctx, tx, hks, m.Quote)
	if err != nil {
		return err
	}
	after := map[string]interface{}{
		"base": m.Base, "quote": m.Quote, "status": string(m.Status), "min_order_size": m.MinOrderSize,
		"max_order_size": m.MaxOrderSize, "min_notional": m.MinNotional, "tick_size": m.TickSize, "lot_size": m.LotSize,
		"maker_fee_bps": m.MakerFeeBPS, "taker_fee_bps": m.TakerFeeBPS,
	}
	var id string
	var status, minOrder, maxOrder, minNotional, tick, lot string
	var maker, taker int
	err = tx.QueryRowContext(ctx, `SELECT id, status::text, min_order_size::text, max_order_size::text, min_notional::text,
			tick_size::text, lot_size::text, maker_fee_bps, taker_fee_bps
		FROM markets WHERE base_currency_hk = $1 AND quote_currency_hk = $2 FOR UPDATE`, base, quote).
		Scan(&id, &status, &minOrder, &maxOrder, &minNotional, &tick, &lot, &maker, &taker)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	changes := &audit.Changes{After: after}
	if found {
		changes = changedValues(map[string]interface{}{
			"base": m.Base, "quote": m.Quote, "status": status, "min_order_size": minOrder, "max_order_size": maxOrder,
			"min_notional": minNotional, "tick_size": tick, "lot_size": lot, "maker_fee_bps": maker, "taker_fee_bps": taker,
		}, after)
		if len(changes.After) == 0 {
			report.Markets.Unchanged++
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE markets SET status = $2::market_status, min_order_size = $3::numeric,
				max_order_size = $4::numeric, min_notional = $5::numeric, tick_size = $6::numeric, lot_size = $7::numeric,
				maker_fee_bps = $8, taker_fee_bps = $9, updated_at = NOW()
			WHERE id = $1`, id, string(m.Status), m.MinOrderSize, m.MaxOrderSize, m.MinNotional, m.TickSize, m.LotSize,
			m.MakerFeeBPS, m.TakerFeeBPS)
		report.Markets.Updated++
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO markets (base_currency_hk, quote_currency_hk, status, min_order_size,
				max_order_size, min_notional, tick_size, lot_size, maker_fee_bps, taker_fee_bps)
			VALUES ($1, $2, $3::market_status, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8::numeric, $9, $10)
			RETURNING id`, base, quote, string(m.Status), m.MinOrderSize, m.MaxOrderSize, m.MinNotional, m.TickSize, m.LotSize,
			m.MakerFeeBPS, m.TakerFeeBPS).Scan(&id)
		report.Markets.Created++
	}
	if err != nil {
		return err
	}
	e := audit.Of(origin, audit.ActionMarketSet, audit.TargetMarket, id)
	e.Changes = changes
	if err := audit.Record(ctx, tx, e); err != nil {
		return fmt.Errorf("%w: %s of %s: %v", ErrUnrecorded, audit.ActionMarketSet, id, err)
	}
	return nil
}
