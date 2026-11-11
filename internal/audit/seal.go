package audit

import (
	"context"
	"database/sql"
	"log"
	"sync/atomic"
)

// Sealer seals the audit events when they are written, and checks their
// seals when the log is read (#337, #139). internal/rowseal implements it
// with the row-mac data keys; this package cannot import rowseal, which
// records its own findings in the audit log.
type Sealer interface {
	// EventFields returns the expressions of the canonical text of the
	// sealed columns of audit_events, in their order, qualified by alias.
	EventFields(alias string) []string
	// SealEvent returns the seal of a new event, whose sealed columns have
	// the canonical text values, and the version of its key.
	SealEvent(hk string, values []sql.NullString) ([]byte, int64, error)
	// CheckEvent compares an event that was read with its seal: "" when
	// they match, and otherwise the verdict, such as "mismatch".
	CheckEvent(ctx context.Context, hk string, values []sql.NullString, seal []byte, keyVersion sql.NullInt64) (string, error)
}

// The integrity of an event, as the admin query reports it.
const (
	// IntegrityVerified is an event that matches its seal.
	IntegrityVerified = "verified"
	// IntegrityMismatch is an event that does not match its seal, or whose
	// seal names a key that does not exist. Only a change made without the
	// backend causes either.
	IntegrityMismatch = "mismatch"
	// IntegrityUnsealed is an event written without a seal: before
	// migration 11, or by a server without data keys.
	IntegrityUnsealed = "unsealed"
	// IntegrityUnchecked is a sealed event that this instance cannot check:
	// it has no data keys, or the key service did not answer.
	IntegrityUnchecked = "unchecked"
)

// verdictMissing is the verdict of Sealer.CheckEvent for an event without a
// seal, rowseal's CheckMissing. Every other verdict means a change.
const verdictMissing = "missing"

type sealerHolder struct{ s Sealer }

var activeSealer atomic.Pointer[sealerHolder]

// UseSealer seals every event recorded from now on with s, and checks the
// seals of the events that List returns; nil stops both. It returns a
// function that restores the previous sealer, for tests. The server calls it
// once its data keys are loaded; without them, events have no seal.
func UseSealer(s Sealer) (restore func()) {
	var next *sealerHolder
	if s != nil {
		next = &sealerHolder{s: s}
	}
	previous := activeSealer.Swap(next)
	return func() { activeSealer.Store(previous) }
}

// currentSealer returns the sealer in use, or nil.
func currentSealer() Sealer {
	if h := activeSealer.Load(); h != nil {
		return h.s
	}
	return nil
}

// integrity checks the seal of an event that was read.
func integrity(ctx context.Context, s Sealer, hk string, values []sql.NullString, seal []byte, keyVersion sql.NullInt64) string {
	if len(seal) == 0 || !keyVersion.Valid {
		return IntegrityUnsealed
	}
	if s == nil {
		return IntegrityUnchecked
	}
	verdict, err := s.CheckEvent(ctx, hk, values, seal, keyVersion)
	switch {
	case err != nil:
		log.Printf("SECURITY: the seal of audit event %s could not be checked: %v", hk, err)
		return IntegrityUnchecked
	case verdict == "":
		return IntegrityVerified
	case verdict == verdictMissing:
		return IntegrityUnsealed
	default:
		log.Printf("SECURITY: audit event %s does not match its seal (%s)", hk, verdict)
		return IntegrityMismatch
	}
}
