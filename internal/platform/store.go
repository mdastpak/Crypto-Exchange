package platform

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/google/uuid"
)

// Limits of the fields of a control.
const (
	MaxMessage = 500
	MaxReason  = 1000
	MaxNote    = 1000
	// MaxAllowed bounds each allow list of a maintenance.
	MaxAllowed = 50
	// pastTolerance is how far in the past a start may be, for the clock
	// of the client; such a start is now.
	pastTolerance = time.Minute
	// maxList bounds the controls that a list returns.
	maxList = 200
)

// Errors of the changes.
var (
	ErrNotFound      = errors.New("control not found")
	ErrEnded         = errors.New("control already ended or lifted")
	ErrSameApprover  = errors.New("the lift must be approved by another administrator")
	errMalformedData = errors.New("malformed control row")
)

// VersionConflict is a change based on another version of the control.
type VersionConflict struct{ Current int64 }

func (v *VersionConflict) Error() string {
	return fmt.Sprintf("the control is at version %d", v.Current)
}

// Invalid is a control that fails validation: field errors by field name.
type Invalid map[string]string

func (v Invalid) Error() string {
	parts := make([]string, 0, len(v))
	for field, problem := range v {
		parts = append(parts, field+" "+problem)
	}
	slices.Sort(parts)
	return "invalid control: " + strings.Join(parts, "; ")
}

// columns are the columns that scan reads, in its order, on a row named c.
const columns = `c.hk, c.kind, c.feature, c.starts_at, c.ends_at, c.announce_at, COALESCE(c.message_fa, ''),
	COALESCE(c.message_en, ''), c.reason, array_to_string(c.allow_networks::TEXT[], ','),
	array_to_string(c.allow_users::TEXT[], ','), c.created_by, c.created_at, c.lift_requested_by,
	c.lift_requested_at, c.lifted_at, c.lifted_by, COALESCE(c.lift_note, ''), c.version,
	(c.lifted_at IS NULL AND c.starts_at <= NOW() AND (c.ends_at IS NULL OR c.ends_at > NOW()))`

// openSQL is the condition of a control that applies now or later.
const openSQL = `c.lifted_at IS NULL AND (c.ends_at IS NULL OR c.ends_at > NOW())`

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (*Control, error) {
	c := &Control{}
	var feature, createdBy, liftRequestedBy, liftedBy sql.NullString
	var endsAt, liftRequestedAt, liftedAt sql.NullTime
	var networks, users string
	err := row.Scan(&c.HK, &c.Kind, &feature, &c.StartsAt, &endsAt, &c.AnnounceAt, &c.Message.FA, &c.Message.EN,
		&c.Reason, &networks, &users, &createdBy, &c.CreatedAt, &liftRequestedBy, &liftRequestedAt, &liftedAt,
		&liftedBy, &c.LiftNote, &c.Version, &c.Active)
	if err != nil {
		return nil, err
	}
	if feature.Valid {
		f := Feature(feature.String)
		c.Feature = &f
	}
	c.EndsAt = timePtr(endsAt)
	c.LiftRequestedAt = timePtr(liftRequestedAt)
	c.LiftedAt = timePtr(liftedAt)
	c.CreatedBy = stringPtr(createdBy)
	c.LiftRequestedBy = stringPtr(liftRequestedBy)
	c.LiftedBy = stringPtr(liftedBy)
	c.AllowNetworks = splitList(networks)
	c.AllowUsers = splitList(users)
	if err := c.parseNetworks(); err != nil {
		return nil, fmt.Errorf("%w: %v", errMalformedData, err)
	}
	return c, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func stringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func splitList(s string) []string {
	list := []string{}
	for _, v := range strings.Split(s, ",") {
		if v != "" {
			list = append(list, v)
		}
	}
	return list
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Querier runs queries: a *sql.DB, or the *sql.Tx of an operation.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func query(ctx context.Context, q Querier, sqlText string, args ...any) ([]*Control, error) {
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read the platform controls: %w", err)
	}
	defer rows.Close()
	list := []*Control{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("failed to read the platform controls: %w", err)
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// Open returns the controls that apply now or later, by start.
func Open(ctx context.Context, q Querier) ([]*Control, error) {
	return query(ctx, q, `SELECT `+columns+` FROM platform_controls c WHERE `+openSQL+` ORDER BY c.starts_at, c.hk`)
}

// List returns the controls, newest first, at most limit (and at most 200).
func List(ctx context.Context, q Querier, limit int) ([]*Control, error) {
	if limit < 1 || limit > maxList {
		limit = maxList
	}
	return query(ctx, q, `SELECT `+columns+` FROM platform_controls c ORDER BY c.created_at DESC, c.hk DESC LIMIT $1`, limit)
}

// Get returns one control.
func Get(ctx context.Context, q Querier, hk string) (*Control, error) {
	if _, err := uuid.Parse(hk); err != nil {
		return nil, ErrNotFound
	}
	c, err := scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM platform_controls c WHERE c.hk = $1`, hk))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the platform control: %w", err)
	}
	return c, nil
}

// Load returns the version of the controls and the controls that apply now
// or later. The version is read first, so that a change that commits
// between the two reads is seen again at the next look at the version.
func Load(ctx context.Context, q Querier) (int64, []*Control, error) {
	version, err := Version(ctx, q)
	if err != nil {
		return 0, nil, err
	}
	controls, err := Open(ctx, q)
	if err != nil {
		return 0, nil, err
	}
	return version, controls, nil
}

// Version returns the version of the controls, which every change raises.
func Version(ctx context.Context, q Querier) (int64, error) {
	var version int64
	if err := q.QueryRowContext(ctx, `SELECT version FROM platform_state`).Scan(&version); err != nil {
		return 0, fmt.Errorf("failed to read the version of the platform controls: %w", err)
	}
	return version, nil
}

// Check returns a *Locked when a control closes use for subject. An
// operation that changes money calls it in its transaction: it takes a
// share lock of platform_state, which a change of a control waits for, so
// a control created while the operation runs applies to it or waits until
// it commits. A control that starts on schedule applies to the checks
// after its start, by the database's clock.
func Check(ctx context.Context, tx *sql.Tx, use Feature, subject Subject) error {
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp() FROM platform_state FOR SHARE`).Scan(&now); err != nil {
		return fmt.Errorf("failed to lock the platform state: %w", err)
	}
	controls, err := query(ctx, tx, `SELECT `+columns+` FROM platform_controls c
		WHERE c.lifted_at IS NULL AND c.starts_at <= $1 AND (c.ends_at IS NULL OR c.ends_at > $1)`, now)
	if err != nil {
		return err
	}
	if locked := evaluate(controls, now, use, subject); locked != nil {
		return locked
	}
	return nil
}

// lockState takes the exclusive lock of platform_state, which waits for
// the operations in flight that checked the controls.
func lockState(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM platform_state FOR UPDATE`); err != nil {
		return fmt.Errorf("failed to lock the platform state: %w", err)
	}
	return nil
}

// bumpState raises the version that the instances watch.
func bumpState(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE platform_state SET version = version + 1, updated_at = NOW()`); err != nil {
		return fmt.Errorf("failed to update the platform state: %w", err)
	}
	return nil
}

// New is a control to create.
type New struct {
	Kind    Kind
	Feature Feature
	// StartsAt is now when nil. AnnounceAt is the creation when nil.
	StartsAt      *time.Time
	EndsAt        *time.Time
	AnnounceAt    *time.Time
	Message       Message
	Reason        string
	AllowNetworks []string
	AllowUsers    []string
	// CreatedBy is the administrator, empty for cmd/admin.
	CreatedBy string
}

func validText(s string, maxRunes int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxRunes && !strings.ContainsRune(s, 0)
}

// Validate checks n at now, and normalizes it: trimmed texts, a start in
// the recent past moved to now, and canonical networks.
func (n *New) Validate(now time.Time) error {
	errs := Invalid{}
	switch n.Kind {
	case KindMaintenance:
		if n.Feature != "" {
			errs["feature"] = "must be empty for a maintenance, which closes everything"
		}
	case KindLockdown:
		if !n.Feature.Valid() {
			errs["feature"] = "must be one of " + featureList()
		}
	default:
		errs["kind"] = "must be maintenance or lockdown"
	}
	start := now
	if n.StartsAt != nil {
		switch {
		case n.StartsAt.Before(now.Add(-pastTolerance)):
			errs["starts_at"] = "must not be in the past"
		case n.StartsAt.After(now):
			start = *n.StartsAt
		default:
			n.StartsAt = nil
		}
	}
	if n.EndsAt != nil && !n.EndsAt.After(start) {
		errs["ends_at"] = "must be after the start, and in the future"
	}
	if n.AnnounceAt != nil {
		switch {
		case n.AnnounceAt.After(start):
			errs["announce_at"] = "must not be after the start"
		case !n.AnnounceAt.After(now):
			n.AnnounceAt = nil
		}
	}
	n.Message.FA, n.Message.EN = strings.TrimSpace(n.Message.FA), strings.TrimSpace(n.Message.EN)
	if !validText(n.Message.FA, MaxMessage) {
		errs["message.fa"] = fmt.Sprintf("must be at most %d characters", MaxMessage)
	}
	if !validText(n.Message.EN, MaxMessage) {
		errs["message.en"] = fmt.Sprintf("must be at most %d characters", MaxMessage)
	}
	n.Reason = strings.TrimSpace(n.Reason)
	if n.Reason == "" || !validText(n.Reason, MaxReason) {
		errs["reason"] = fmt.Sprintf("is required, at most %d characters", MaxReason)
	}
	if n.Kind != KindMaintenance && (len(n.AllowNetworks) > 0 || len(n.AllowUsers) > 0) {
		errs["allow"] = "only a maintenance has allow lists"
	}
	if len(n.AllowNetworks) > MaxAllowed {
		errs["allow_networks"] = fmt.Sprintf("must have at most %d networks", MaxAllowed)
	} else {
		for i, raw := range n.AllowNetworks {
			network, err := parseNetwork(raw)
			if err != nil {
				errs["allow_networks"] = "must be IP addresses or CIDR networks, such as 203.0.113.0/24"
				break
			}
			n.AllowNetworks[i] = network
		}
	}
	if len(n.AllowUsers) > MaxAllowed {
		errs["allow_users"] = fmt.Sprintf("must have at most %d users", MaxAllowed)
	} else {
		for _, u := range n.AllowUsers {
			if _, err := uuid.Parse(u); err != nil {
				errs["allow_users"] = "must be the UUIDs of users"
				break
			}
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// parseNetwork returns raw, an address or a network, as a canonical
// network.
func parseNetwork(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if ip := net.ParseIP(raw); ip != nil {
		if ip.To4() != nil {
			return ip.String() + "/32", nil
		}
		return ip.String() + "/128", nil
	}
	_, n, err := net.ParseCIDR(raw)
	if err != nil {
		return "", err
	}
	return n.String(), nil
}

func featureList() string {
	names := make([]string, len(Features))
	for i, f := range Features {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// inTx runs change in a transaction that holds the exclusive lock of
// platform_state, and raises its version when change made a change.
func inTx(ctx context.Context, db *sql.DB, change func(tx *sql.Tx) (bool, error)) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start the change of the platform controls: %w", err)
	}
	defer tx.Rollback()
	if err := lockState(ctx, tx); err != nil {
		return err
	}
	changed, err := change(tx)
	if err != nil {
		return err
	}
	if changed {
		if err := bumpState(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit the change of the platform controls: %w", err)
	}
	return nil
}

func summary(c *Control) map[string]interface{} {
	m := map[string]interface{}{"control": c.HK, "kind": c.Kind, "starts_at": c.StartsAt.UTC().Format(time.RFC3339)}
	if c.Feature != nil {
		m["feature"] = *c.Feature
	}
	if c.EndsAt != nil {
		m["ends_at"] = c.EndsAt.UTC().Format(time.RFC3339)
	}
	return m
}

// Create records a control, and its audit event from origin, in one
// transaction. n must be valid.
func Create(ctx context.Context, db *sql.DB, n New, origin audit.Event) (*Control, error) {
	if err := n.Validate(time.Now()); err != nil {
		return nil, err
	}
	var created *Control
	err := inTx(ctx, db, func(tx *sql.Tx) (bool, error) {
		var feature any
		if n.Kind == KindLockdown {
			feature = string(n.Feature)
		}
		networks := n.AllowNetworks
		if networks == nil {
			networks = []string{}
		}
		users := n.AllowUsers
		if users == nil {
			users = []string{}
		}
		c, err := scan(tx.QueryRowContext(ctx, `
			INSERT INTO platform_controls AS c (hk, kind, feature, starts_at, ends_at, announce_at, message_fa, message_en,
				reason, allow_networks, allow_users, created_by)
			VALUES ($1, $2, $3, COALESCE($4, NOW()), $5, LEAST(COALESCE($6, NOW()), COALESCE($4, NOW())), $7, $8, $9,
				COALESCE(string_to_array(NULLIF($10, ''), ','), '{}')::CIDR[],
				COALESCE(string_to_array(NULLIF($11, ''), ','), '{}')::UUID[], $12)
			RETURNING `+columns,
			uuid.NewString(), n.Kind, feature, n.StartsAt, n.EndsAt, n.AnnounceAt, nullable(n.Message.FA),
			nullable(n.Message.EN), n.Reason, strings.Join(networks, ","), strings.Join(users, ","), nullable(n.CreatedBy)))
		if err != nil {
			return false, fmt.Errorf("failed to record the platform control: %w", err)
		}
		after := summary(c)
		after["reason"] = c.Reason
		if len(c.AllowNetworks) > 0 {
			after["allow_networks"] = c.AllowNetworks
		}
		if len(c.AllowUsers) > 0 {
			after["allow_users"] = c.AllowUsers
		}
		event := audit.Of(origin, audit.ActionPlatformControlCreate, audit.TargetPlatformControl, c.HK)
		event.Changes = &audit.Changes{After: after}
		if err := audit.Record(ctx, tx, event); err != nil {
			return false, err
		}
		created = c
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// lockControl reads a control for a change, in tx, and checks its version.
func lockControl(ctx context.Context, tx *sql.Tx, hk string, expected *int64) (*Control, error) {
	if _, err := uuid.Parse(hk); err != nil {
		return nil, ErrNotFound
	}
	c, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM platform_controls c WHERE c.hk = $1 FOR UPDATE`, hk))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the platform control: %w", err)
	}
	if expected != nil && *expected != c.Version {
		return nil, &VersionConflict{Current: c.Version}
	}
	return c, nil
}

// Changes are the fields of a control that can change: its end, to extend
// or shorten it, and its message. A nil field stays.
type Changes struct {
	EndsAt  *time.Time
	Message *Message
}

// Update changes the end or the message of a control that has not ended,
// with an audit event from origin. expected, when set, is the version the
// change is based on.
func Update(ctx context.Context, db *sql.DB, hk string, changes Changes, expected *int64, origin audit.Event) (*Control, error) {
	errs := Invalid{}
	if changes.EndsAt == nil && changes.Message == nil {
		errs["body"] = "must change ends_at or message"
	}
	if changes.Message != nil {
		m := Message{FA: strings.TrimSpace(changes.Message.FA), EN: strings.TrimSpace(changes.Message.EN)}
		if !validText(m.FA, MaxMessage) {
			errs["message.fa"] = fmt.Sprintf("must be at most %d characters", MaxMessage)
		}
		if !validText(m.EN, MaxMessage) {
			errs["message.en"] = fmt.Sprintf("must be at most %d characters", MaxMessage)
		}
		changes.Message = &m
	}
	if changes.EndsAt != nil && !changes.EndsAt.After(time.Now()) {
		errs["ends_at"] = "must be in the future"
	}
	if len(errs) > 0 {
		return nil, errs
	}

	var updated *Control
	err := inTx(ctx, db, func(tx *sql.Tx) (bool, error) {
		c, err := lockControl(ctx, tx, hk, expected)
		if err != nil {
			return false, err
		}
		if c.endedAt(time.Now()) {
			return false, ErrEnded
		}
		if changes.EndsAt != nil && !changes.EndsAt.After(c.StartsAt) {
			return false, Invalid{"ends_at": "must be after the start"}
		}
		message := c.Message
		if changes.Message != nil {
			message = *changes.Message
		}
		endsAt := c.EndsAt
		if changes.EndsAt != nil {
			endsAt = changes.EndsAt
		}
		u, err := scan(tx.QueryRowContext(ctx, `
			UPDATE platform_controls AS c SET ends_at = $2, message_fa = $3, message_en = $4, version = version + 1
			WHERE c.hk = $1 RETURNING `+columns, c.HK, endsAt, nullable(message.FA), nullable(message.EN)))
		if err != nil {
			return false, fmt.Errorf("failed to change the platform control: %w", err)
		}
		before, after := summary(c), summary(u)
		if changes.Message != nil {
			before["message"], after["message"] = c.Message, u.Message
		}
		event := audit.Of(origin, audit.ActionPlatformControlUpdate, audit.TargetPlatformControl, c.HK)
		event.Changes = &audit.Changes{Before: before, After: after}
		if err := audit.Record(ctx, tx, event); err != nil {
			return false, err
		}
		updated = u
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// LiftRequest ends a control.
type LiftRequest struct {
	HK   string
	Note string
	// By is the administrator, empty for cmd/admin, whose operator the
	// audit log names.
	By string
	// Expected, when set, is the version the lift is based on.
	Expected *int64
	// Approval are the features whose active lockdowns an administrator
	// lifts only with a second one: the first asks, and another approves.
	// cmd/admin, which needs access to the database, lifts at once.
	Approval []Feature
}

// LiftResult is the outcome of a lift.
type LiftResult struct {
	Control *Control
	// Pending is a lift that waits for a second administrator.
	Pending bool
}

// Lift ends a control now, with an audit event from origin, or records the
// request of a lift that needs a second administrator. A control that has
// not started is cancelled without approval.
func Lift(ctx context.Context, db *sql.DB, r LiftRequest, origin audit.Event) (LiftResult, error) {
	r.Note = strings.TrimSpace(r.Note)
	if r.Note == "" || !validText(r.Note, MaxNote) {
		return LiftResult{}, Invalid{"note": fmt.Sprintf("is required, at most %d characters", MaxNote)}
	}
	var result LiftResult
	err := inTx(ctx, db, func(tx *sql.Tx) (bool, error) {
		c, err := lockControl(ctx, tx, r.HK, r.Expected)
		if err != nil {
			return false, err
		}
		now := time.Now()
		if c.endedAt(now) {
			return false, ErrEnded
		}
		needsApproval := r.By != "" && c.Kind == KindLockdown && c.activeAt(now) && slices.Contains(r.Approval, c.feature())
		if needsApproval && c.LiftRequestedBy == nil {
			u, err := scan(tx.QueryRowContext(ctx, `
				UPDATE platform_controls AS c SET lift_requested_by = $2, lift_requested_at = NOW(), version = version + 1
				WHERE c.hk = $1 RETURNING `+columns, c.HK, r.By))
			if err != nil {
				return false, fmt.Errorf("failed to record the lift request: %w", err)
			}
			event := audit.Of(origin, audit.ActionPlatformControlLiftRequest, audit.TargetPlatformControl, c.HK)
			event.Changes = &audit.Changes{After: map[string]interface{}{"control": c.HK, "note": r.Note}}
			if err := audit.Record(ctx, tx, event); err != nil {
				return false, err
			}
			result = LiftResult{Control: u, Pending: true}
			// Nothing changes for users yet.
			return false, nil
		}
		if needsApproval && *c.LiftRequestedBy == r.By {
			return false, ErrSameApprover
		}
		u, err := scan(tx.QueryRowContext(ctx, `
			UPDATE platform_controls AS c SET lifted_at = NOW(), lifted_by = $2, lift_note = $3, version = version + 1
			WHERE c.hk = $1 RETURNING `+columns, c.HK, nullable(r.By), r.Note))
		if err != nil {
			return false, fmt.Errorf("failed to lift the platform control: %w", err)
		}
		before, after := summary(c), summary(u)
		before["lifted"], after["lifted"], after["note"] = false, true, r.Note
		if needsApproval {
			after["requested_by"] = *c.LiftRequestedBy
		}
		event := audit.Of(origin, audit.ActionPlatformControlLift, audit.TargetPlatformControl, c.HK)
		event.Changes = &audit.Changes{Before: before, After: after}
		if err := audit.Record(ctx, tx, event); err != nil {
			return false, err
		}
		result = LiftResult{Control: u}
		return true, nil
	})
	if err != nil {
		return LiftResult{}, err
	}
	return result, nil
}
