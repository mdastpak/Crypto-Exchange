package currencies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	domain "github.com/mdastpak/crypto-exchange/internal/domain/currency"
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/jackc/pgx/v5/pgconn"
)

// The registry of how the currencies move and trade (#57): the networks of
// each currency, in currency_networks, and the markets of two currencies.
// The import of the registry file and the admin routes write them through
// the same functions, each change with its audit event in its transaction.

// Errors of the changes of networks and markets.
var (
	// ErrContractFixed refuses a change of the contract or the decimals of
	// a network that exists. Deposits on the network are read by its
	// contract, so a new one would credit another token as this currency,
	// and new decimals would scale every amount on it. A token that moved
	// to a new contract is a migration of the currency (#215); the
	// reviewed registry file can still change them (#309).
	ErrContractFixed = errors.New("the contract and decimals of a network do not change once it exists")
	// ErrContractTaken refuses a contract that is another currency's on the
	// same network.
	ErrContractTaken = errors.New("the contract is another currency's on this network")
	// ErrMarketNotFound answers a market that does not exist.
	ErrMarketNotFound = errors.New("market not found")
)

// Invalid refuses a network or a market that fails its checks, with the
// problems by field.
type Invalid struct {
	Problems []assetregistry.Problem
}

func (e *Invalid) Error() string {
	texts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		texts[i] = p.Text
	}
	return strings.Join(texts, "; ")
}

// writeResult is what a write did to a row.
type writeResult int

const (
	rowUnchanged writeResult = iota
	rowCreated
	rowUpdated
)

// writeNetwork writes the network n of the currency hk, with decimals and
// the currency of its fee, and records its audit event. With fixed, a
// change of the contract or the decimals of a network that exists is
// refused with ErrContractFixed.
func writeNetwork(ctx context.Context, tx *sql.Tx, hk string, n assetregistry.Network, decimals int, feeHK string, origin audit.Event, fixed bool) (writeResult, error) {
	after := map[string]interface{}{
		"network": n.Network, "contract_address": n.ContractAddress, "decimals": decimals,
		"custody_mode": string(n.CustodyMode), "deposit_enabled": n.DepositEnabled, "withdrawal_enabled": n.WithdrawalEnabled,
		"min_deposit": n.MinDepositOrZero(), "min_withdrawal": n.MinWithdrawal, "max_withdrawal": n.MaxWithdrawal,
		"withdrawal_fee": n.WithdrawalFee, "fee_currency_hk": feeHK, "requires_tag": n.RequiresTag, "explorer_url": n.ExplorerURL,
	}
	var stored storedNetwork
	sealed := rowseal.CurrencyNetworks.NewRow()
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(contract_address, ''), decimals, custody_mode::text, deposit_enabled, withdrawal_enabled,
			min_deposit::text, min_withdrawal::text, max_withdrawal::text, withdrawal_fee::text, COALESCE(fee_currency_hk::text, ''),
			requires_tag, COALESCE(explorer_url, ''), `+rowseal.CurrencyNetworks.Fields("")+`
		FROM currency_networks WHERE currency_hk = $1 AND network = $2 FOR UPDATE`, hk, n.Network).
		Scan(append([]any{&stored.contract, &stored.decimals, &stored.custodyMode, &stored.depositEnabled, &stored.withdrawalEnabled,
			&stored.minDeposit, &stored.minWithdrawal, &stored.maxWithdrawal, &stored.withdrawalFee, &stored.feeHK,
			&stored.requiresTag, &stored.explorerURL}, sealed.Dest()...)...)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rowUnchanged, err
	}
	var changes *audit.Changes
	if found {
		// A network changed in SQL is not written over: the write would
		// seal what nobody checked (#139). Its finding is resolved first.
		if err := rowseal.Check(ctx, rowseal.CurrencyNetworks, sealed); err != nil {
			return rowUnchanged, err
		}
		if fixed && (stored.contract != n.ContractAddress || stored.decimals != decimals) {
			return rowUnchanged, ErrContractFixed
		}
		changes = changedValues(stored.fields(n.Network), after)
		if len(changes.After) == 0 {
			return rowUnchanged, nil
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
		hk, n.Network, n.ContractAddress, decimals, string(n.CustodyMode), n.DepositEnabled, n.WithdrawalEnabled,
		n.MinDepositOrZero(), n.MinWithdrawal, n.MaxWithdrawal, n.WithdrawalFee, feeHK, n.RequiresTag, n.ExplorerURL)
	if isConstraint(err, "currency_networks_contract") {
		return rowUnchanged, ErrContractTaken
	}
	if err != nil {
		return rowUnchanged, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.CurrencyNetworks, "currency_hk = $1 AND network = $2", hk, n.Network); err != nil {
		return rowUnchanged, err
	}
	result := rowUpdated
	if !found {
		result, changes = rowCreated, &audit.Changes{After: after}
	}
	return result, record(ctx, tx, origin, audit.ActionCurrencyNetworkSet, hk, changes)
}

// isConstraint reports whether err is a violation of the unique constraint
// or index name.
func isConstraint(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == name
}

// writeMarket writes the market m of the currencies base and quote, and
// records its audit event. It returns the ID of the market.
func writeMarket(ctx context.Context, tx *sql.Tx, base, quote string, m assetregistry.Market, origin audit.Event) (string, writeResult, error) {
	after := map[string]interface{}{
		"base": m.Base, "quote": m.Quote, "status": string(m.Status), "min_order_size": m.MinOrderSize,
		"max_order_size": m.MaxOrderSize, "min_notional": m.MinNotional, "tick_size": m.TickSize, "lot_size": m.LotSize,
		"maker_fee_bps": m.MakerFeeBPS, "taker_fee_bps": m.TakerFeeBPS,
	}
	var id string
	var status, minOrder, maxOrder, minNotional, tick, lot string
	var maker, taker int
	sealed := rowseal.Markets.NewRow()
	err := tx.QueryRowContext(ctx, `SELECT id, status::text, min_order_size::text, max_order_size::text, min_notional::text,
			tick_size::text, lot_size::text, maker_fee_bps, taker_fee_bps, `+rowseal.Markets.Fields("")+`
		FROM markets WHERE base_currency_hk = $1 AND quote_currency_hk = $2 FOR UPDATE`, base, quote).
		Scan(append([]any{&id, &status, &minOrder, &maxOrder, &minNotional, &tick, &lot, &maker, &taker}, sealed.Dest()...)...)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", rowUnchanged, err
	}
	changes := &audit.Changes{After: after}
	result := rowCreated
	if found {
		// A market changed in SQL is not written over (#139).
		if err := rowseal.Check(ctx, rowseal.Markets, sealed); err != nil {
			return "", rowUnchanged, err
		}
		changes = changedValues(map[string]interface{}{
			"base": m.Base, "quote": m.Quote, "status": status, "min_order_size": minOrder, "max_order_size": maxOrder,
			"min_notional": minNotional, "tick_size": tick, "lot_size": lot, "maker_fee_bps": maker, "taker_fee_bps": taker,
		}, after)
		if len(changes.After) == 0 {
			return id, rowUnchanged, nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE markets SET status = $2::market_status, min_order_size = $3::numeric,
				max_order_size = $4::numeric, min_notional = $5::numeric, tick_size = $6::numeric, lot_size = $7::numeric,
				maker_fee_bps = $8, taker_fee_bps = $9, updated_at = NOW()
			WHERE id = $1`, id, string(m.Status), m.MinOrderSize, m.MaxOrderSize, m.MinNotional, m.TickSize, m.LotSize,
			m.MakerFeeBPS, m.TakerFeeBPS)
		result = rowUpdated
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO markets (base_currency_hk, quote_currency_hk, status, min_order_size,
				max_order_size, min_notional, tick_size, lot_size, maker_fee_bps, taker_fee_bps)
			VALUES ($1, $2, $3::market_status, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8::numeric, $9, $10)
			RETURNING id`, base, quote, string(m.Status), m.MinOrderSize, m.MaxOrderSize, m.MinNotional, m.TickSize, m.LotSize,
			m.MakerFeeBPS, m.TakerFeeBPS).Scan(&id)
	}
	if err != nil {
		return "", rowUnchanged, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Markets, "id = $1", id); err != nil {
		return "", rowUnchanged, err
	}
	e := audit.Of(origin, audit.ActionMarketSet, audit.TargetMarket, id)
	e.Changes = changes
	if err := audit.Record(ctx, tx, e); err != nil {
		return "", rowUnchanged, fmt.Errorf("%w: %s of %s: %v", ErrUnrecorded, audit.ActionMarketSet, id, err)
	}
	return id, result, nil
}

// NetworkWritten is the outcome of a change of a network.
type NetworkWritten struct {
	Network domain.Network
	// Created reports a new network, and Changed any change.
	Created, Changed bool
	// Public reports a network of a public currency, which the cached list
	// shows: the caller drops it.
	Public bool
}

// SetNetwork adds or changes the network n.Network of the currency hk, with
// decimals, as an administrator (#57). The network is checked as the
// registry file is (assetregistry.CheckNetwork); its fee currency is named
// by symbol. The contract and the decimals of a network that exists do not
// change (ErrContractFixed). It returns ErrNotFound for a currency that does
// not exist or is deleted, an *Invalid for a network that fails its checks,
// and ErrContractTaken for another currency's contract.
func (s Store) SetNetwork(ctx context.Context, hk string, n assetregistry.Network, decimals int, origin audit.Event) (NetworkWritten, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return NetworkWritten{}, err
	}
	defer tx.Rollback()
	var symbol, status string
	var fiat bool
	err = tx.QueryRowContext(ctx, `SELECT info->>'symbol', COALESCE((info->>'if_fiat')::boolean, false), status
		FROM currencies WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`, hk).Scan(&symbol, &fiat, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkWritten{}, ErrNotFound
	}
	if err != nil {
		return NetworkWritten{}, err
	}
	problems := append(assetregistry.CheckNetwork(symbol, fiat, n), assetregistry.CheckDecimals(decimals)...)
	feeHK := ""
	if n.FeeAsset != "" {
		feeHK, err = currencyOf(ctx, tx, nil, n.FeeAsset)
		if errors.Is(err, ErrImport) {
			problems = append(problems, assetregistry.Problem{Field: "fee_asset", Text: "no currency " + n.FeeAsset})
		} else if err != nil {
			return NetworkWritten{}, err
		}
	}
	if len(problems) > 0 {
		return NetworkWritten{}, &Invalid{Problems: problems}
	}
	result, err := writeNetwork(ctx, tx, hk, n, decimals, feeHK, origin, true)
	if err != nil {
		return NetworkWritten{}, err
	}
	written := NetworkWritten{Created: result == rowCreated, Changed: result != rowUnchanged, Public: status == models.CurrencyStatusApproved}
	if written.Network, err = network(ctx, tx, hk, n.Network); err != nil {
		return NetworkWritten{}, err
	}
	return written, tx.Commit()
}

// networkColumns are the columns of a currency.Network.
const networkColumns = `currency_hk, network, COALESCE(contract_address, ''), decimals, custody_mode::text, deposit_enabled,
	withdrawal_enabled, min_deposit, min_withdrawal, max_withdrawal, withdrawal_fee, fee_currency_hk, requires_tag,
	COALESCE(explorer_url, ''), created_at, updated_at`

func scanNetwork(scan func(...any) error) (domain.Network, error) {
	var n domain.Network
	err := scan(&n.CurrencyHK, &n.Network, &n.ContractAddress, &n.Decimals, &n.CustodyMode, &n.DepositEnabled, &n.WithdrawalEnabled,
		&n.MinDeposit, &n.MinWithdrawal, &n.MaxWithdrawal, &n.WithdrawalFee, &n.FeeCurrencyHK, &n.RequiresTag, &n.ExplorerURL,
		&n.CreatedAt, &n.UpdatedAt)
	return n, err
}

// queryer reads in a transaction or out of one.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func network(ctx context.Context, q queryer, hk, name string) (domain.Network, error) {
	return scanNetwork(q.QueryRowContext(ctx, `SELECT `+networkColumns+` FROM currency_networks
		WHERE currency_hk = $1 AND network = $2`, hk, name).Scan)
}

// Networks returns the networks of the currency hk, deleted or not, by
// name. It returns ErrNotFound for a currency that does not exist.
func (s Store) Networks(ctx context.Context, hk string) ([]domain.Network, error) {
	var exists bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM currencies WHERE hk = $1)`, hk).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	//nolint:gosec // G202: the columns are constants of this package and rowseal; the values are arguments.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+networkColumns+`, `+rowseal.CurrencyNetworks.Fields("n")+`
		FROM currency_networks n WHERE n.currency_hk = $1 ORDER BY n.network`, hk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []domain.Network{}
	for rows.Next() {
		sealed := rowseal.CurrencyNetworks.NewRow()
		n, err := scanNetwork(func(dest ...any) error { return rows.Scan(append(dest, sealed.Dest()...)...) })
		if err != nil {
			return nil, err
		}
		// In enforce mode, a network changed in SQL makes the list
		// rowseal.ErrUnverified, as the other admin views of sealed rows
		// do; its finding names it.
		if err := rowseal.Check(ctx, rowseal.CurrencyNetworks, sealed); err != nil {
			return nil, err
		}
		list = append(list, n)
	}
	return list, rows.Err()
}

// MarketWritten is the outcome of a change of a market.
type MarketWritten struct {
	Market market.Market
	// Created reports a new market, and Changed any change.
	Created, Changed bool
}

// SetMarket adds or changes the market of m.Base and m.Quote, currencies
// named by symbol, as an administrator (#57). The market is checked as the
// registry file is (assetregistry.CheckMarket). It returns an *Invalid for
// a market that fails its checks or names a currency that does not exist.
func (s Store) SetMarket(ctx context.Context, m assetregistry.Market, origin audit.Event) (MarketWritten, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return MarketWritten{}, err
	}
	defer tx.Rollback()
	problems := assetregistry.CheckMarket(m)
	hks := map[string]string{}
	for _, side := range []struct{ field, symbol string }{{"base", m.Base}, {"quote", m.Quote}} {
		hk, err := currencyOf(ctx, tx, nil, side.symbol)
		if errors.Is(err, ErrImport) {
			problems = append(problems, assetregistry.Problem{Field: side.field, Text: "no currency " + side.symbol})
			continue
		}
		if err != nil {
			return MarketWritten{}, err
		}
		hks[side.field] = hk
	}
	if len(problems) > 0 {
		return MarketWritten{}, &Invalid{Problems: problems}
	}
	id, result, err := writeMarket(ctx, tx, hks["base"], hks["quote"], m, origin)
	if err != nil {
		return MarketWritten{}, err
	}
	written := MarketWritten{Created: result == rowCreated, Changed: result != rowUnchanged}
	if written.Market, err = scanMarket(tx.QueryRowContext(ctx, marketQuery+` WHERE m.id = $1`, id).Scan); err != nil {
		return MarketWritten{}, err
	}
	return written, tx.Commit()
}

// marketColumns and marketFrom read markets with the symbols of their
// currencies.
const (
	marketColumns = `m.id, m.base_currency_hk, m.quote_currency_hk, b.info->>'symbol', q.info->>'symbol', m.status::text,
	m.min_order_size, m.max_order_size, m.min_notional, m.tick_size, m.lot_size, m.maker_fee_bps, m.taker_fee_bps,
	m.max_position_size, m.max_leverage, m.market_maker_fee_bps, m.price_band_bps, m.created_at, m.updated_at`
	marketFrom  = ` FROM markets m JOIN currencies b ON b.hk = m.base_currency_hk JOIN currencies q ON q.hk = m.quote_currency_hk`
	marketQuery = `SELECT ` + marketColumns + marketFrom
)

// sealedMarketQuery also reads the seal of each market.
var sealedMarketQuery = `SELECT ` + marketColumns + `, ` + rowseal.Markets.Fields("m") + marketFrom

func scanMarket(scan func(...any) error) (market.Market, error) {
	var m market.Market
	var status string
	err := scan(&m.ID, &m.BaseCurrencyHK, &m.QuoteCurrencyHK, &m.BaseSymbol, &m.QuoteSymbol, &status,
		&m.MinOrderSize, &m.MaxOrderSize, &m.MinNotional, &m.TickSize, &m.LotSize, &m.MakerFeeBPS, &m.TakerFeeBPS,
		&m.MaxPositionSize, &m.MaxLeverage, &m.MarketMakerFeeBPS, &m.PriceBandBPS, &m.CreatedAt, &m.UpdatedAt)
	m.Status = market.MarketStatus(status)
	return m, err
}

// scanSealedMarket reads a row of sealedMarketQuery and checks its seal: a
// market changed in SQL returns rowseal.ErrUnverified in enforce mode.
func scanSealedMarket(ctx context.Context, scan func(...any) error) (market.Market, error) {
	sealed := rowseal.Markets.NewRow()
	m, err := scanMarket(func(dest ...any) error { return scan(append(dest, sealed.Dest()...)...) })
	if err != nil {
		return m, err
	}
	return m, rowseal.Check(ctx, rowseal.Markets, sealed)
}

// Markets returns the markets that are not deleted, by symbol. In enforce
// mode, a market that fails its seal makes it rowseal.ErrUnverified, as
// the other admin views of sealed rows do; its finding names it.
func (s Store) Markets(ctx context.Context) ([]market.Market, error) {
	rows, err := s.DB.QueryContext(ctx, sealedMarketQuery+` WHERE m.deleted_at IS NULL
		ORDER BY b.info->>'symbol', q.info->>'symbol'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []market.Market{}
	for rows.Next() {
		m, err := scanSealedMarket(ctx, rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// Market returns the market of the currencies base and quote, named by
// symbol, or ErrMarketNotFound, or rowseal.ErrUnverified for one that fails
// its seal in enforce mode.
func (s Store) Market(ctx context.Context, base, quote string) (market.Market, error) {
	m, err := scanSealedMarket(ctx, s.DB.QueryRowContext(ctx, sealedMarketQuery+` WHERE m.deleted_at IS NULL
		AND b.info->>'symbol' = $1 AND b.deleted_at IS NULL AND q.info->>'symbol' = $2 AND q.deleted_at IS NULL`, base, quote).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrMarketNotFound
	}
	return m, err
}
