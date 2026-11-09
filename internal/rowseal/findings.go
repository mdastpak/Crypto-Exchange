package rowseal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// Finding is a row that did not match its seal.
type Finding struct {
	ID         int64      `json:"id"`
	Table      string     `json:"table"`
	RowKey     string     `json:"row_key"`
	Check      string     `json:"check" enums:"missing,mismatch,unknown_key"`
	DetectedBy string     `json:"detected_by" enums:"read,verifier"`
	DetectedAt time.Time  `json:"detected_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	SeenCount  int64      `json:"seen_count"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Resolution string     `json:"resolution,omitempty" enums:"resealed,report_mode,row_gone"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// ErrResolved reports a finding that is already resolved.
var ErrResolved = errors.New("the finding is already resolved")

// MaxReasonLength bounds the reason of a re-seal, as the table does.
const MaxReasonLength = 1000

const findingFields = `id, table_name, row_key, check_failed, detected_by, detected_at, last_seen_at, seen_count,
	resolved_at, COALESCE(resolution, ''), COALESCE(resolved_by::text, ''), COALESCE(reason, '')`

func scanFinding(scan func(...any) error) (Finding, error) {
	var f Finding
	var resolved sql.NullTime
	err := scan(&f.ID, &f.Table, &f.RowKey, &f.Check, &f.DetectedBy, &f.DetectedAt, &f.LastSeenAt, &f.SeenCount,
		&resolved, &f.Resolution, &f.ResolvedBy, &f.Reason)
	if resolved.Valid {
		f.ResolvedAt = &resolved.Time
	}
	return f, err
}

// ListOptions select findings.
type ListOptions struct {
	// All includes the resolved findings; otherwise only open ones.
	All bool
	// BeforeID lists findings older than it, for the next page; 0 starts
	// at the newest.
	BeforeID int64
	// Limit is the page size.
	Limit int
}

// ListFindings returns findings, newest first.
func ListFindings(ctx context.Context, q Querier, o ListOptions) ([]Finding, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	rows, err := q.QueryContext(ctx, `SELECT `+findingFields+` FROM integrity_findings
		WHERE ($1 OR resolved_at IS NULL) AND ($2 = 0 OR id < $2)
		ORDER BY id DESC LIMIT $3`, o.All, o.BeforeID, o.Limit)
	if err != nil {
		return nil, fmt.Errorf("list the integrity findings: %w", err)
	}
	defer rows.Close()
	findings := []Finding{}
	for rows.Next() {
		f, err := scanFinding(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list the integrity findings: %w", err)
		}
		findings = append(findings, f)
	}
	return findings, rows.Err()
}

// GetFinding returns one finding.
func GetFinding(ctx context.Context, q Querier, id int64) (Finding, error) {
	f, err := scanFinding(q.QueryRowContext(ctx, `SELECT `+findingFields+` FROM integrity_findings WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, ErrNotFound
	}
	return f, err
}

// resolve closes the open finding of a row.
func resolve(ctx context.Context, q Querier, id int64, resolution, resolvedBy, reason string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE integrity_findings SET resolved_at = NOW(), resolution = $2,
			resolved_by = NULLIF($3, '')::uuid, reason = NULLIF($4, '')
		WHERE id = $1 AND resolved_at IS NULL`, id, resolution, resolvedBy, reason)
	return err
}

// ResealFinding re-seals the row of an open finding after an administrator
// reviewed it, and closes the finding, in one transaction with its audit
// event. origin is the administrator or the operator, and reason says why
// the current values are right, such as a reviewed data fix. The event
// shows the values that are sealed now, without the secret ones. A row
// that no longer exists closes the finding as row_gone.
func (s *Sealer) ResealFinding(ctx context.Context, id int64, origin audit.Event, reason string) (Finding, error) {
	reason = strings.TrimSpace(reason)
	if s == nil {
		return Finding{}, errors.New("row seals need keys.provider")
	}
	if reason == "" || len(reason) > MaxReasonLength {
		return Finding{}, fmt.Errorf("a reason of 1 to %d characters is required", MaxReasonLength)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Finding{}, err
	}
	defer tx.Rollback()
	f, err := scanFinding(tx.QueryRowContext(ctx, `SELECT `+findingFields+` FROM integrity_findings WHERE id = $1 FOR UPDATE`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Finding{}, ErrNotFound
	}
	if err != nil {
		return Finding{}, err
	}
	if f.ResolvedAt != nil {
		return Finding{}, ErrResolved
	}
	t := TableNamed(f.Table)
	if t == nil {
		return Finding{}, fmt.Errorf("finding %d names the unknown table %q", id, f.Table)
	}
	where := t.Key + " = $1::" + t.KeyType
	if err := s.Reseal(ctx, tx, t, where, f.RowKey); err != nil {
		return Finding{}, err
	}
	r := t.NewRow()
	//nolint:gosec // G202: the table and its columns are constants of this package.
	err = tx.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM `+t.Name+` WHERE `+where, f.RowKey).Scan(r.Dest()...)
	resolution := "resealed"
	var after map[string]interface{}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		resolution = "row_gone"
	case err != nil:
		return Finding{}, err
	default:
		after = t.Shown(r)
	}
	if err := resolve(ctx, tx, id, resolution, origin.ActorID, reason); err != nil {
		return Finding{}, err
	}
	e := audit.Of(origin, audit.ActionIntegrityReseal, t.Name, f.RowKey)
	e.Changes = &audit.Changes{
		Before: map[string]interface{}{"finding": id, "check": f.Check},
		After:  map[string]interface{}{"resolution": resolution, "reason": reason, "values": after},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Finding{}, err
	}
	if err := tx.Commit(); err != nil {
		return Finding{}, err
	}
	return GetFinding(ctx, s.db, id)
}

// verifierLockID is the advisory lock that one instance at a time holds
// while it verifies a batch.
const verifierLockID = 0x726f777365616c // "rowseal"

// VerifyBatchSize is how many rows a batch of the verifier reads.
const VerifyBatchSize = 500

// Report counts what a verifier pass did.
type Report struct {
	Checked int
	// Findings are the rows that did not match their seal.
	Findings int
	// Resealed are the findings re-sealed in report mode.
	Resealed int
	// Rotated are the rows that matched a seal of an older key version,
	// sealed again with the current one.
	Rotated int
	// Skipped is set when another instance held the lock.
	Skipped bool
}

// VerifyAll checks every row of the protected tables, a batch at a time,
// each batch in a transaction under an advisory lock, so that two instances
// do not verify at once; a pass that another instance holds the lock for is
// skipped. A row that does not match its seal is recorded as a finding; in
// report mode, it is also sealed again, and its finding closed as
// report_mode. A row sealed with an older version of the row-mac key is
// sealed again with the current one, so that the older version can be
// retired after a rotation.
func (s *Sealer) VerifyAll(ctx context.Context) (Report, error) {
	var report Report
	if s == nil {
		return report, nil
	}
	for _, t := range Tables {
		after := ""
		for {
			next, done, err := s.verifyBatch(ctx, t, after, &report)
			if err != nil || report.Skipped {
				return report, err
			}
			if done {
				break
			}
			after = next
		}
	}
	return report, nil
}

// verifyBatch checks the rows of t after the key after, and returns the
// last key it read and whether t is done.
func (s *Sealer) verifyBatch(ctx context.Context, t *Table, after string, report *Report) (string, bool, error) {
	current, err := s.keys.Current(keys.PurposeRowMAC)
	if err != nil {
		return "", false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, verifierLockID).Scan(&locked); err != nil {
		return "", false, err
	}
	if !locked {
		report.Skipped = true
		return "", false, nil
	}
	//nolint:gosec // G202: the table and its columns are constants of this package; values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT `+t.Fields("")+` FROM `+t.Name+`
		WHERE $1 = '' OR `+t.Key+` > $1::`+t.KeyType+` ORDER BY `+t.Key+` LIMIT $2`, after, VerifyBatchSize)
	if err != nil {
		return "", false, err
	}
	var batch []*Row
	for rows.Next() {
		r := t.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			rows.Close()
			return "", false, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	where := t.Key + " = $1::" + t.KeyType + " AND row_version = $2"
	for _, r := range batch {
		report.Checked++
		v, err := s.verdict(ctx, t, r)
		if err != nil {
			return "", false, err
		}
		if v == "" {
			if r.KeyVersion.Int64 < int64(current.Version()) {
				if err := s.Reseal(ctx, tx, t, where, r.Key, r.Version); err != nil {
					return "", false, err
				}
				report.Rotated++
			}
			continue
		}
		report.Findings++
		id, created, err := recordFinding(ctx, tx, t, r.Key, v, DetectedByVerifier)
		if err != nil {
			return "", false, err
		}
		if created {
			s.logf("SECURITY: %s %s does not match its seal (%s); finding %d, mode %s", t.Name, r.Key, v, id, s.mode)
			e := audit.Of(audit.Event{}, audit.ActionIntegritySealMismatch, t.Name, r.Key)
			e.Outcome, e.Reason = audit.Failure, string(v)
			e.Changes = &audit.Changes{After: map[string]interface{}{"finding": id, "detected_by": DetectedByVerifier, "mode": s.mode}}
			if err := audit.Record(ctx, tx, e); err != nil {
				return "", false, err
			}
		}
		if s.mode == ModeReport {
			// The version in the condition leaves a row that a request
			// wrote meanwhile to that write, which sealed it.
			if err := s.Reseal(ctx, tx, t, where, r.Key, r.Version); err != nil {
				return "", false, err
			}
			if err := resolve(ctx, tx, id, "report_mode", "", "sealed by the verifier in report mode"); err != nil {
				return "", false, err
			}
			report.Resealed++
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	if len(batch) < VerifyBatchSize {
		return "", true, nil
	}
	return batch[len(batch)-1].Key, false, nil
}

// Verifier runs VerifyAll every interval until Stop.
type Verifier struct {
	sealer   *Sealer
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewVerifier returns a verifier of s; Start runs it.
func NewVerifier(s *Sealer, interval time.Duration) *Verifier {
	return &Verifier{sealer: s, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start runs a pass at once, and then every interval.
func (v *Verifier) Start() {
	go func() {
		defer close(v.done)
		ticker := time.NewTicker(v.interval)
		defer ticker.Stop()
		for {
			v.pass()
			select {
			case <-ticker.C:
			case <-v.stop:
				return
			}
		}
	}()
}

func (v *Verifier) pass() {
	ctx, cancel := context.WithTimeout(context.Background(), v.interval)
	defer cancel()
	go func() {
		select {
		case <-v.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	report, err := v.sealer.VerifyAll(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		v.sealer.logf("The row seal verifier failed: %v", err)
	case report.Findings > 0 || report.Rotated > 0:
		v.sealer.logf("The row seal verifier checked %d rows: %d findings, %d re-sealed in report mode, %d sealed with the current key",
			report.Checked, report.Findings, report.Resealed, report.Rotated)
	}
}

// Stop ends the verifier and waits for a pass in progress, until ctx ends.
func (v *Verifier) Stop(ctx context.Context) error {
	select {
	case <-v.stop:
	default:
		close(v.stop)
	}
	select {
	case <-v.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
