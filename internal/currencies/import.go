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

// count counts a row that a write created, changed, or left.
func (c *ImportCounts) count(result writeResult) {
	switch result {
	case rowCreated:
		c.Created++
	case rowUpdated:
		c.Updated++
	default:
		c.Unchanged++
	}
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
	return s.apply(ctx, reg, origin, dryRun, false)
}

// Sync adds to the currencies what the registry file reg has and they lack
// (#57): the import at startup of assets.auto_sync. It creates the
// currencies, networks, and markets that do not exist, as Import does, and
// never changes one that exists, so that a restart does not undo what
// administrators changed, such as a network they closed. An entry that it
// cannot add, such as a currency whose symbol a deleted currency holds or a
// rebrand reserved, or a contract that another currency has, is left out
// with a note, so that one entry does not keep the servers from starting. A
// file that fails its checks is refused whole.
func (s Store) Sync(ctx context.Context, reg *assetregistry.Registry, origin audit.Event) (ImportReport, error) {
	return s.apply(ctx, reg, origin, false, true)
}

// importRun is one import or sync, in its transaction.
type importRun struct {
	tx     *sql.Tx
	origin audit.Event
	report ImportReport
	// sync adds what is missing, and notes what it cannot add.
	sync bool
	// hks are the currencies of the assets of the file, by symbol.
	hks map[string]string
}

// rule is what the writes of the run do with a row that exists.
func (r *importRun) rule() rowRule {
	if r.sync {
		return keepRow
	}
	return replaceRow
}

func (s Store) apply(ctx context.Context, reg *assetregistry.Registry, origin audit.Event, dryRun, sync bool) (ImportReport, error) {
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
	r := &importRun{tx: tx, origin: origin, sync: sync, hks: make(map[string]string, len(reg.Assets))}
	for _, a := range reg.Assets {
		if err := r.tolerate(ctx, a.Symbol, func() error { return r.importCurrency(ctx, a) }); err != nil {
			return ImportReport{}, err
		}
	}
	for _, a := range reg.Assets {
		if r.hks[a.Symbol] == "" {
			// Left out by the sync, with a note.
			continue
		}
		for _, n := range a.Networks {
			where := fmt.Sprintf("%s on %s", a.Symbol, n.Network)
			if err := r.tolerate(ctx, where, func() error { return r.importNetwork(ctx, a, n) }); err != nil {
				return ImportReport{}, fmt.Errorf("%s: %w", where, err)
			}
		}
	}
	for _, m := range reg.Markets {
		where := fmt.Sprintf("market %s/%s", m.Base, m.Quote)
		if err := r.tolerate(ctx, where, func() error { return r.importMarket(ctx, m) }); err != nil {
			return ImportReport{}, fmt.Errorf("%s: %w", where, err)
		}
	}
	if dryRun {
		return r.report, nil
	}
	return r.report, tx.Commit()
}

// tolerate runs write. In a sync, it runs in a savepoint: a write that
// fails is undone, noted as left out under what, and the sync goes on.
// Otherwise, its error fails the import.
func (r *importRun) tolerate(ctx context.Context, what string, write func() error) error {
	if !r.sync {
		return write()
	}
	if _, err := r.tx.ExecContext(ctx, `SAVEPOINT registry_sync`); err != nil {
		return err
	}
	if err := write(); err != nil {
		if _, undo := r.tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT registry_sync`); undo != nil {
			return fmt.Errorf("%w (undoing it: %v)", err, undo)
		}
		r.report.Notes = append(r.report.Notes, fmt.Sprintf("%s: left out: %v", what, err))
		return nil
	}
	_, err := r.tx.ExecContext(ctx, `RELEASE SAVEPOINT registry_sync`)
	return err
}

// importCurrency finds or creates the currency of asset a, and keeps its
// hk.
func (r *importRun) importCurrency(ctx context.Context, a assetregistry.Asset) error {
	info := infoOf(a)
	var hk, enName, faName string
	err := r.tx.QueryRowContext(ctx, `SELECT hk, info->>'en_name', info->>'fa_name' FROM currencies
		WHERE info->>'symbol' = $1 AND deleted_at IS NULL FOR UPDATE`, a.Symbol).Scan(&hk, &enName, &faName)
	if err == nil {
		r.hks[a.Symbol] = hk
		r.report.Existing = append(r.report.Existing, a.Symbol)
		if enName != info.EnName || faName != info.FaName {
			r.report.Notes = append(r.report.Notes, fmt.Sprintf("%s: the names differ from the file's; the admin API changes them", a.Symbol))
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	status, err := statusOf(a.Status)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Symbol, err)
	}
	if problems := Validate(status, info); problems != nil {
		return fmt.Errorf("%w: %s: %s", ErrImport, a.Symbol, describe(problems))
	}
	w, err := create(ctx, r.tx, status, info, r.origin)
	if err != nil {
		return fmt.Errorf("%s: %w", a.Symbol, err)
	}
	r.hks[a.Symbol] = w.HK
	r.report.Created = append(r.report.Created, a.Symbol)
	return nil
}

// importNetwork writes the network n of asset a to currency_networks.
func (r *importRun) importNetwork(ctx context.Context, a assetregistry.Asset, n assetregistry.Network) error {
	feeHK := ""
	if n.FeeAsset != "" {
		var err error
		if feeHK, err = currencyOf(ctx, r.tx, r.hks, n.FeeAsset); err != nil {
			return err
		}
	}
	// The reviewed file may change a contract or decimals, which the admin
	// API may not (ErrContractFixed); the sync changes nothing.
	result, err := writeNetwork(ctx, r.tx, r.hks[a.Symbol], n, a.Decimals, feeHK, r.origin, r.rule())
	if err != nil {
		return err
	}
	r.report.Networks.count(result)
	return nil
}

// importMarket writes the market m to markets.
func (r *importRun) importMarket(ctx context.Context, m assetregistry.Market) error {
	base, err := currencyOf(ctx, r.tx, r.hks, m.Base)
	if err != nil {
		return err
	}
	quote, err := currencyOf(ctx, r.tx, r.hks, m.Quote)
	if err != nil {
		return err
	}
	_, result, err := writeMarket(ctx, r.tx, base, quote, m, r.origin, r.rule())
	if err != nil {
		return err
	}
	r.report.Markets.count(result)
	return nil
}
