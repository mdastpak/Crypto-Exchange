package kyc

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// stored is an application as read: its review, and its sealed form.
type stored struct {
	Review
	form []byte
}

// read reads an application and its documents, each checked against its
// seal.
func (s Service) read(ctx context.Context, q querier, id string) (stored, error) {
	return s.readApplication(ctx, q, id, "")
}

// lock reads an application as read does, locked for update.
func (s Service) lock(ctx context.Context, tx *sql.Tx, id string) (stored, error) {
	return s.readApplication(ctx, tx, id, " FOR UPDATE")
}

func (s Service) readApplication(ctx context.Context, q querier, id, lock string) (stored, error) {
	t := rowseal.KYCApplications
	r := t.NewRow()
	var a stored
	var form []byte
	var notes sql.NullString
	var submitted, reviewed sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT form_data_enc, review_notes, submitted_at, reviewed_at, created_at, `+t.Fields("")+`
		FROM kyc_applications WHERE id = $1`+lock, id).
		Scan(append([]any{&form, &notes, &submitted, &reviewed, &a.CreatedAt}, r.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("kyc: read the application: %w", err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return a, err
	}
	col := func(name string) string { return r.Values[t.Index(name)].String }
	a.ID, a.UserHK, a.Status, a.Reason = r.Key, col("user_id"), Status(col("status")), col("reason")
	a.Level, a.ReviewedBy, a.ReviewNotes, a.form = 2, col("reviewed_by"), notes.String, form
	a.CreatedAt = a.CreatedAt.UTC()
	if submitted.Valid {
		at := submitted.Time.UTC()
		a.SubmittedAt = &at
	}
	if reviewed.Valid {
		at := reviewed.Time.UTC()
		a.ReviewedAt = &at
	}
	a.Documents, err = documents(ctx, q, id)
	return a, err
}

// storedDocument is a document as read: with where its content is.
type storedDocument struct {
	Document
	application string
	key         string
	digest      []byte
}

// documentSelect starts a read of documents; their sealed fields follow.
const documentSelect = `SELECT created_at, `

func scanDocument(ctx context.Context, row interface{ Scan(...any) error }) (storedDocument, error) {
	t := rowseal.KYCDocuments
	r := t.NewRow()
	var d storedDocument
	if err := row.Scan(append([]any{&d.CreatedAt}, r.Dest()...)...); err != nil {
		return d, err
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return d, err
	}
	col := func(name string) string { return r.Values[t.Index(name)].String }
	d.ID, d.Type, d.MimeType = r.Key, DocumentType(col("type")), col("mime_type")
	d.application, d.key = col("application_id"), col("storage_path")
	d.CreatedAt = d.CreatedAt.UTC()
	var err error
	if d.Size, err = strconv.ParseInt(col("file_size"), 10, 64); err != nil {
		return d, err
	}
	if d.digest, err = hex.DecodeString(col("content_digest")); err != nil {
		return d, err
	}
	return d, nil
}

// documents reads the documents of an application, the oldest first.
func documents(ctx context.Context, q querier, id string) ([]Document, error) {
	rows, err := q.QueryContext(ctx, documentSelect+rowseal.KYCDocuments.Fields("")+`
		FROM kyc_documents WHERE application_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("kyc: read the documents: %w", err)
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDocument(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d.Document)
	}
	return out, rows.Err()
}

// document reads one document, checked against its seal.
func (s Service) document(ctx context.Context, id string) (storedDocument, error) {
	if _, err := uuid.Parse(id); err != nil {
		return storedDocument{}, ErrNotFound
	}
	d, err := scanDocument(ctx, s.DB.QueryRowContext(ctx, documentSelect+rowseal.KYCDocuments.Fields("")+`
		FROM kyc_documents WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}
