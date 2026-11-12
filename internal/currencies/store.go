package currencies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/models"
)

// Store administers the currencies (#48): each change writes the currency,
// the event of a change to the public view in the outbox, and the audit
// event of the change, in one transaction, so that no change is made
// without its event and its record. The handlers of the admin routes use it
// through an interface.
type Store struct {
	DB *sql.DB
}

// Errors of the changes of a Store.
var (
	// ErrAlreadyDeleted refuses the delete of a deleted currency.
	ErrAlreadyDeleted = errors.New("the currency is already deleted")
	// ErrNotDeleted refuses the restore of a currency that is not deleted.
	ErrNotDeleted = errors.New("the currency is not deleted")
	// ErrNotPreviousSymbol refuses the release of a symbol that the
	// currency never had, or that is released already.
	ErrNotPreviousSymbol = errors.New("not a previous symbol of the currency")
	// ErrSymbolCurrent refuses the release of the symbol in use.
	ErrSymbolCurrent = errors.New("the symbol is the current one")
	// ErrUnrecorded refuses a change whose audit event could not be
	// written: the change is rolled back with it.
	ErrUnrecorded = errors.New("the change could not be recorded")
)

// VersionConflict refuses a change based on a version of the currency that
// is no longer the current one.
type VersionConflict struct {
	Current int64
}

func (v *VersionConflict) Error() string {
	return fmt.Sprintf("the currency is at version %d", v.Current)
}

// Written is the outcome of a change.
type Written struct {
	HK      string
	Version int64
	// Public reports a change to the public view, which the caller makes
	// visible at once: it drops the cached list and nudges the outbox relay.
	Public bool
}

// Edit applies a request to the current status and info of a currency,
// and returns the new ones, or an error that refuses the update, which
// Update returns as it is.
type Edit func(status string, info models.CurrencyInfo) (string, models.CurrencyInfo, error)

// record writes the audit event of a change from origin in tx.
func record(ctx context.Context, tx *sql.Tx, origin audit.Event, action, hk string, changes *audit.Changes) error {
	e := audit.Of(origin, action, audit.TargetCurrency, hk)
	e.Changes = changes
	if err := audit.Record(ctx, tx, e); err != nil {
		return fmt.Errorf("%w: %s of %s: %v", ErrUnrecorded, action, hk, err)
	}
	return nil
}

// Fields are the fields of a currency as the audit log records them.
func Fields(status string, info models.CurrencyInfo) map[string]interface{} {
	return map[string]interface{}{
		"status": status, "en_name": info.EnName, "fa_name": info.FaName, "symbol": info.Symbol,
		"if_fiat": info.IfFiat, "is_token": info.IsToken, "contract": info.Contract, "logo_url": info.LogoURL,
	}
}

// ChangedFields keeps the fields whose values differ between before and
// after.
func ChangedFields(before, after map[string]interface{}) *audit.Changes {
	changes := &audit.Changes{Before: map[string]interface{}{}, After: map[string]interface{}{}}
	for name, value := range after {
		if before[name] != value {
			changes.Before[name], changes.After[name] = before[name], value
		}
	}
	return changes
}

// Create records a new currency, valid already, with the audit event from
// origin.
func (s Store) Create(ctx context.Context, status string, info models.CurrencyInfo, origin audit.Event) (Written, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Written{}, err
	}
	defer tx.Rollback()
	w, err := create(ctx, tx, status, info, origin)
	if err != nil {
		return Written{}, err
	}
	return w, tx.Commit()
}

// create inserts a new currency in tx, with the event of the public view
// and the audit event from origin.
func create(ctx context.Context, tx *sql.Tx, status string, info models.CurrencyInfo, origin audit.Event) (Written, error) {
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return Written{}, fmt.Errorf("encode the currency: %w", err)
	}
	var w Written
	if err := tx.QueryRowContext(ctx, "INSERT INTO currencies (status, info) VALUES ($1, $2::jsonb) RETURNING hk, version",
		status, infoJSON).Scan(&w.HK, &w.Version); err != nil {
		return Written{}, err
	}
	if w.Public, err = EnqueueEvent(ctx, tx, false, models.Currency{HK: w.HK, Status: status, Info: info}); err != nil {
		return Written{}, err
	}
	if err := record(ctx, tx, origin, audit.ActionCurrencyCreate, w.HK, &audit.Changes{After: Fields(status, info)}); err != nil {
		return Written{}, err
	}
	return w, nil
}

// Update applies edit to a currency that is not deleted, with the audit
// event of the fields it changed. The row is locked from the read to the
// write, so that concurrent partial updates of one currency are applied one
// after the other instead of overwriting each other's fields. expected,
// when set, is the version the update is based on.
func (s Store) Update(ctx context.Context, hk string, expected *int64, edit Edit, origin audit.Event) (Written, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Written{}, err
	}
	defer tx.Rollback()
	var status string
	var infoData []byte
	var version int64
	err = tx.QueryRowContext(ctx, "SELECT status, info, version FROM currencies WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE", hk).
		Scan(&status, &infoData, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Written{}, ErrNotFound
	}
	if err != nil {
		return Written{}, err
	}
	if expected != nil && *expected != version {
		return Written{}, &VersionConflict{Current: version}
	}
	var info models.CurrencyInfo
	if err := json.Unmarshal(infoData, &info); err != nil {
		return Written{}, fmt.Errorf("decode currency %s: %w", hk, err)
	}
	wasPublic := status == models.CurrencyStatusApproved
	before := Fields(status, info)
	if status, info, err = edit(status, info); err != nil {
		return Written{}, err
	}
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return Written{}, fmt.Errorf("encode currency %s: %w", hk, err)
	}
	w := Written{HK: hk}
	if err := tx.QueryRowContext(ctx,
		"UPDATE currencies SET status=$1, info=$2::jsonb, version=version+1, updated_at=NOW() WHERE hk=$3 AND deleted_at IS NULL RETURNING version",
		status, infoJSON, hk).Scan(&w.Version); err != nil {
		return Written{}, err
	}
	if w.Public, err = EnqueueEvent(ctx, tx, wasPublic, models.Currency{HK: hk, Status: status, Info: info}); err != nil {
		return Written{}, err
	}
	if err := record(ctx, tx, origin, audit.ActionCurrencyUpdate, hk, ChangedFields(before, Fields(status, info))); err != nil {
		return Written{}, err
	}
	return w, tx.Commit()
}

// Delete soft-deletes a currency. The row is locked from the read to the
// write, so that the previous status, which decides the event, is the one
// this delete replaces, and concurrent deletes succeed only once.
func (s Store) Delete(ctx context.Context, hk string, expected *int64, origin audit.Event) (Written, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Written{}, err
	}
	defer tx.Rollback()
	var deleted bool
	var status string
	var version int64
	err = tx.QueryRowContext(ctx, "SELECT (deleted_at IS NOT NULL), status, version FROM currencies WHERE hk = $1 FOR UPDATE", hk).
		Scan(&deleted, &status, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Written{}, ErrNotFound
	}
	if err != nil {
		return Written{}, err
	}
	if deleted {
		return Written{}, ErrAlreadyDeleted
	}
	if expected != nil && *expected != version {
		return Written{}, &VersionConflict{Current: version}
	}
	w := Written{HK: hk}
	if err := tx.QueryRowContext(ctx,
		"UPDATE currencies SET deleted_at=NOW(), status='deleted', version=version+1 WHERE hk=$1 RETURNING version", hk).Scan(&w.Version); err != nil {
		return Written{}, err
	}
	if w.Public, err = EnqueueEvent(ctx, tx, status == models.CurrencyStatusApproved,
		models.Currency{HK: hk, Status: models.CurrencyStatusDeleted}); err != nil {
		return Written{}, err
	}
	if err := record(ctx, tx, origin, audit.ActionCurrencyDelete, hk, &audit.Changes{
		Before: map[string]interface{}{"status": status},
		After:  map[string]interface{}{"status": models.CurrencyStatusDeleted},
	}); err != nil {
		return Written{}, err
	}
	return w, tx.Commit()
}

// Restore undoes a delete: the currency keeps its hk, symbol, and contract,
// and comes back as pending, which is not public, so the restore publishes
// no event. The row is locked as in Delete.
func (s Store) Restore(ctx context.Context, hk string, expected *int64, origin audit.Event) (Written, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Written{}, err
	}
	defer tx.Rollback()
	var deleted bool
	var version int64
	err = tx.QueryRowContext(ctx, "SELECT (deleted_at IS NOT NULL), version FROM currencies WHERE hk = $1 FOR UPDATE", hk).
		Scan(&deleted, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Written{}, ErrNotFound
	}
	if err != nil {
		return Written{}, err
	}
	if expected != nil && *expected != version {
		return Written{}, &VersionConflict{Current: version}
	}
	if !deleted {
		return Written{}, ErrNotDeleted
	}
	w := Written{HK: hk}
	if err := tx.QueryRowContext(ctx,
		"UPDATE currencies SET deleted_at=NULL, status=$1, version=version+1, updated_at=NOW() WHERE hk=$2 RETURNING version",
		models.CurrencyStatusPending, hk).Scan(&w.Version); err != nil {
		return Written{}, err
	}
	if err := record(ctx, tx, origin, audit.ActionCurrencyRestore, hk, &audit.Changes{
		Before: map[string]interface{}{"status": models.CurrencyStatusDeleted},
		After:  map[string]interface{}{"status": models.CurrencyStatusPending},
	}); err != nil {
		return Written{}, err
	}
	return w, tx.Commit()
}

// ReleaseSymbol frees a previous symbol of a currency, which a rebrand
// reserved (#215), for other currencies. The row stays, marked, for the
// record (#248). Public reports a currency in the public view, whose list
// the caller drops.
func (s Store) ReleaseSymbol(ctx context.Context, hk, symbol string, origin audit.Event) (Written, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Written{}, err
	}
	defer tx.Rollback()
	var status string
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT status, deleted_at FROM currencies WHERE hk = $1 FOR UPDATE`, hk).Scan(&status, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Written{}, ErrNotFound
	}
	var current bool
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT current FROM currency_symbols WHERE symbol = $1 AND currency_hk = $2 AND released_at IS NULL`,
			symbol, hk).Scan(&current)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Written{}, ErrNotPreviousSymbol
	case err != nil:
		return Written{}, err
	case current:
		return Written{}, ErrSymbolCurrent
	}
	if _, err := tx.ExecContext(ctx, `UPDATE currency_symbols SET released_at = NOW(), released_by = $3
		WHERE symbol = $1 AND currency_hk = $2 AND NOT current AND released_at IS NULL`, symbol, hk, origin.ActorID); err != nil {
		return Written{}, err
	}
	if err := record(ctx, tx, origin, audit.ActionCurrencySymbolRelease, hk,
		&audit.Changes{Before: map[string]any{"previous_symbol": symbol}}); err != nil {
		return Written{}, err
	}
	return Written{HK: hk, Public: status == models.CurrencyStatusApproved && !deleted.Valid}, tx.Commit()
}

// detailQuery reads currencies with their versions and lifecycles, deleted
// ones included.
const detailQuery = `SELECT c.id, c.hk, c.status, c.info, c.version, c.created_at, c.updated_at, c.deleted_at, ` +
	DetailColumns + ` FROM currencies c`

// scanDetail reads a row of detailQuery.
func scanDetail(scan func(...any) error) (models.Currency, error) {
	var cur models.Currency
	var info []byte
	var created, updated, deleted sql.NullTime
	var lifecycle DetailScan
	if err := scan(append([]any{&cur.ID, &cur.HK, &cur.Status, &info, &cur.Version, &created, &updated, &deleted}, lifecycle.Dest()...)...); err != nil {
		return cur, err
	}
	if err := json.Unmarshal(info, &cur.Info); err != nil {
		return cur, fmt.Errorf("currency %s: %w", cur.HK, err)
	}
	if err := lifecycle.Apply(&cur); err != nil {
		return cur, fmt.Errorf("currency %s: %w", cur.HK, err)
	}
	cur.CreatedAt, cur.UpdatedAt = formatTime(created), formatTime(updated)
	if deleted.Valid {
		at := formatTime(deleted)
		cur.DeletedAt = &at
	}
	return cur, nil
}

// Get returns one currency, whatever its status, with its version.
func (s Store) Get(ctx context.Context, hk string) (models.Currency, error) {
	cur, err := scanDetail(s.DB.QueryRowContext(ctx, detailQuery+` WHERE c.hk = $1`, hk).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return cur, ErrNotFound
	}
	return cur, err
}

// All reads every currency, deleted ones included, for the filters of the
// admin list to run over.
func (s Store) All(ctx context.Context) ([]models.Currency, error) {
	rows, err := s.DB.QueryContext(ctx, detailQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []models.Currency
	for rows.Next() {
		cur, err := scanDetail(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, cur)
	}
	return list, rows.Err()
}

// formatTime formats a database time in RFC 3339, in UTC, or as "" when it
// is NULL.
func formatTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}
