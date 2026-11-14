// Package agreements records which legal documents a user accepted (#316):
// the terms of service, the privacy notice, and whatever else
// legal.documents requires. An account is created only with an acceptance
// of the current version of each, and a user accepts a new version before
// their next change.
package agreements

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
)

// ErrIncomplete reports an acceptance that does not cover the current
// version of every required document.
var ErrIncomplete = errors.New("the acceptance does not cover every required document")

// Document is a legal document in its current version.
type Document struct {
	// Kind names the document, such as "terms" or "privacy".
	Kind string `json:"kind" example:"terms"`
	// Version is its current version.
	Version string `json:"version" example:"2026-11"`
	// URL is where the client shows it.
	URL string `json:"url,omitempty" example:"https://example.com/legal/terms"`
}

// Required returns the documents that legal.documents requires, in their
// current versions. Without any, nothing is asked of a user.
func Required(legal config.LegalConfig) []Document {
	docs := make([]Document, 0, len(legal.Documents))
	for _, d := range legal.Documents {
		docs = append(docs, Document{Kind: d.Kind, Version: d.Version, URL: d.URL})
	}
	return docs
}

// Covers reports whether accept, the versions that a client says its user
// accepted by kind, has the current version of every required document.
func Covers(legal config.LegalConfig, accept map[string]string) bool {
	for _, d := range Required(legal) {
		if strings.TrimSpace(accept[d.Kind]) != d.Version {
			return false
		}
	}
	return true
}

// Querier reads: a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Missing returns the required documents whose current version the user
// has not accepted.
func Missing(ctx context.Context, q Querier, legal config.LegalConfig, userHK string) ([]Document, error) {
	required := Required(legal)
	if len(required) == 0 {
		return []Document{}, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT kind, version FROM user_agreements WHERE user_hk = $1`, userHK)
	if err != nil {
		return nil, fmt.Errorf("read the agreements of a user: %w", err)
	}
	defer rows.Close()
	accepted := map[Document]bool{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.Kind, &d.Version); err != nil {
			return nil, fmt.Errorf("read an agreement: %w", err)
		}
		accepted[d] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the agreements of a user: %w", err)
	}
	missing := slices.DeleteFunc(required, func(d Document) bool {
		return accepted[Document{Kind: d.Kind, Version: d.Version}]
	})
	return missing, nil
}

// Origin is where an acceptance came from.
type Origin struct {
	// Country is the country of the request, or "" when it is not known.
	Country string
	// Event carries the address, the user agent, and the request ID. Its
	// actor is the user.
	Event audit.Event
}

// Record stores, within tx, that the user accepted the current version of
// every required document, and records it in the audit log. accept must
// cover them all: ErrIncomplete otherwise. A version that the user already
// accepted keeps its first acceptance.
func Record(ctx context.Context, tx *sql.Tx, legal config.LegalConfig, userHK string, accept map[string]string, origin Origin) error {
	if !Covers(legal, accept) {
		return ErrIncomplete
	}
	for _, d := range Required(legal) {
		result, err := tx.ExecContext(ctx, `INSERT INTO user_agreements (user_hk, kind, version, ip, user_agent, country)
			VALUES ($1, $2, $3, NULLIF($4, '')::inet, NULLIF($5, ''), NULLIF($6, ''))
			ON CONFLICT (user_hk, kind, version) DO NOTHING`,
			userHK, d.Kind, d.Version, origin.Event.IP, origin.Event.UserAgent, origin.Country)
		if err != nil {
			return fmt.Errorf("store the agreement to %s: %w", d.Kind, err)
		}
		if n, err := result.RowsAffected(); err != nil || n == 0 {
			continue
		}
		event := origin.Event
		event.Action, event.Outcome, event.Reason = audit.ActionUserAgreementAccept, audit.Success, ""
		event.Changes = &audit.Changes{After: map[string]interface{}{"kind": d.Kind, "version": d.Version, "country": origin.Country}}
		event.ActorType, event.ActorID = audit.ActorUser, userHK
		event.TargetType, event.TargetID = audit.TargetUser, userHK
		if err := audit.Record(ctx, tx, event); err != nil {
			return fmt.Errorf("record the agreement to %s: %w", d.Kind, err)
		}
	}
	return nil
}

// SignUpBlocked reports whether legal.signup.blocked_countries refuses new
// accounts from country. A country that is not known is not refused.
func SignUpBlocked(legal config.LegalConfig, country string) bool {
	if country == "" {
		return false
	}
	return slices.ContainsFunc(legal.SignUp.BlockedCountries, func(c string) bool {
		return strings.EqualFold(strings.TrimSpace(c), country)
	})
}

// Store reads and records the acceptances of the users in DB, against the
// legal section of Config, read at each call. A nil Config requires no
// document.
type Store struct {
	DB     *sql.DB
	Config *config.Configuration
}

func (s Store) legal() config.LegalConfig {
	if s.Config == nil {
		return config.LegalConfig{}
	}
	return s.Config.Legal
}

// Missing returns the required documents that the user has not accepted.
func (s Store) Missing(ctx context.Context, userHK string) ([]Document, error) {
	return Missing(ctx, s.DB, s.legal(), userHK)
}

// Accept records, in its own transaction, that the user accepted the
// current version of every required document.
func (s Store) Accept(ctx context.Context, userHK string, accept map[string]string, origin Origin) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recording agreements: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := Record(ctx, tx, s.legal(), userHK, accept, origin); err != nil {
		return err
	}
	return tx.Commit()
}
