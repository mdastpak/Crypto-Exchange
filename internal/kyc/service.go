package kyc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// Service runs the applications.
type Service struct {
	DB    *sql.DB
	Store ObjectStore
	// LinkTTL is how long a signed link to a document works; 0 is a
	// minute.
	LinkTTL time.Duration
	// LinkPath is the path of the content of a document, with %s for its
	// id.
	LinkPath string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Provider verifies the new applications, such as Sumsub; nil is the
	// manual review.
	Provider Provider
}

// providerName is the provider of the new applications.
func (s Service) providerName() string {
	if s.Provider == nil {
		return ProviderManual
	}
	return s.Provider.Name()
}

// ProviderManual names the review by hand.
const ProviderManual = "manual"

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// available refuses while the service has no data keys, which seal the
// personal data, or no storage.
func (s Service) available() (*field.Cipher, error) {
	c := field.Active()
	if c == nil || s.Store == nil {
		return nil, ErrUnavailable
	}
	return c, nil
}

func formPlace(id string) field.Place {
	return field.Place{Table: "kyc_applications", Column: "form_data_enc", Row: id}
}

func contentPlace(id string) field.Place {
	return field.Place{Table: "kyc_documents", Column: "content", Row: id}
}

// lockUser takes the lock of the user's row, first of every lock of a
// change (#405's order: the user, then the rest).
func lockUser(ctx context.Context, tx *sql.Tx, userHK string, share bool) error {
	mode := "FOR UPDATE"
	if share {
		mode = "FOR SHARE"
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE hk = $1 AND deleted_at IS NULL `+mode, userHK).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Apply opens an application of the user for level 2 with form. The user
// must be at level 1, and have no other open application.
func (s Service) Apply(ctx context.Context, userHK string, form Form, origin audit.Event) (Application, error) {
	c, err := s.available()
	if err != nil {
		return Application{}, err
	}
	if err := form.Validate(); err != nil {
		return Application{}, err
	}
	raw, err := json.Marshal(form)
	if err != nil {
		return Application{}, err
	}
	id := uuid.NewString()
	sealed, err := c.Seal(formPlace(id), string(raw))
	if err != nil {
		return Application{}, err
	}
	// A provider opens the verification before the transaction, which
	// waits for no network; a level that changes in between is checked
	// again under the lock, and an applicant that nothing records is
	// harmless.
	var providerRef string
	if s.Provider != nil {
		if err := s.eligible(ctx, userHK); err != nil {
			return Application{}, err
		}
		if providerRef, err = s.Provider.Start(ctx, id); err != nil {
			return Application{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockUser(ctx, tx, userHK, false); err != nil {
		return Application{}, err
	}
	if err := s.eligible(ctx, userHK, tx); err != nil {
		return Application{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kyc_applications (id, user_id, requested_tier, status, form_data_enc, provider, provider_ref_id)
		VALUES ($1, $2, 'STANDARD', 'PENDING', $3, $4, NULLIF($5, ''))`, id, userHK, sealed, s.providerName(), providerRef)
	if isUnique(err, "kyc_applications_open") {
		return Application{}, ErrOpenApplication
	}
	if err != nil {
		return Application{}, fmt.Errorf("kyc: open the application: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.KYCApplications, "id = $1", id); err != nil {
		return Application{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	e := audit.Of(origin, audit.ActionUserKYCApply, audit.TargetKYCApplication, id)
	e.Changes = &audit.Changes{After: map[string]any{"level": 2}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Application{}, err
	}
	a, err := s.read(ctx, tx, id)
	if err != nil {
		return Application{}, err
	}
	return a.Application, tx.Commit()
}

// eligible refuses a user who is not at level 1, read in q, or the
// database.
func (s Service) eligible(ctx context.Context, userHK string, q ...querier) error {
	var conn querier = s.DB
	if len(q) > 0 {
		conn = q[0]
	}
	st, err := verification.Store{}.Level(ctx, conn, userHK)
	if err != nil {
		return err
	}
	switch {
	case st.Level >= verification.Level2:
		return ErrAlreadyVerified
	case st.Level < verification.Level1:
		return ErrLevelRequired
	}
	return nil
}

func isUnique(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}

// Upload adds a document of type to the user's pending application id.
// Its content is checked by what it is, not by its name: a JPEG, a PNG,
// or a PDF of at most MaxDocumentSize. It is encrypted and stored before
// the transaction that records it, so that the transaction waits for no
// network; an object that no row records is removed.
func (s Service) Upload(ctx context.Context, userHK, id string, docType DocumentType, content []byte, origin audit.Event) (Document, error) {
	c, err := s.available()
	if err != nil {
		return Document{}, err
	}
	problems := Invalid{}
	if !slices.Contains(DocumentTypes, docType) {
		problems["type"] = "is not a type of document"
	}
	mime := http.DetectContentType(content)
	switch {
	case len(content) == 0 || len(content) > MaxDocumentSize:
		problems["file"] = fmt.Sprintf("must be 1 byte to %d MB", MaxDocumentSize>>20)
	case !slices.Contains(mimeTypes, mime):
		problems["file"] = "must be a JPEG, a PNG, or a PDF"
	}
	if len(problems) > 0 {
		return Document{}, problems
	}
	if _, err := uuid.Parse(id); err != nil {
		return Document{}, ErrNotFound
	}
	if a, err := s.read(ctx, s.DB, id); err != nil || a.UserHK != userHK {
		return Document{}, notFound(err)
	} else if a.Provider != ProviderManual {
		return Document{}, ErrProviderFlow
	} else if a.Status != StatusPending {
		return Document{}, ErrNotPending
	}

	docID := uuid.NewString()
	sealed, err := c.Seal(contentPlace(docID), string(content))
	if err != nil {
		return Document{}, err
	}
	digest := sha256.Sum256(sealed)
	key := "kyc/" + id + "/" + docID
	if err := s.Store.Put(ctx, key, sealed, "application/octet-stream"); err != nil {
		return Document{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	doc, err := s.record(ctx, userHK, id, docID, docType, mime, len(content), key, digest[:], origin)
	if err != nil {
		if derr := s.Store.Delete(context.WithoutCancel(ctx), key); derr != nil {
			log.Printf("Failed to remove the object of a KYC document that was not recorded: %v", derr)
		}
		return Document{}, err
	}
	return doc, nil
}

// record records a stored document, in one transaction.
func (s Service) record(ctx context.Context, userHK, id, docID string, docType DocumentType, mime string, size int, key string,
	digest []byte, origin audit.Event) (Document, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Document{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockUser(ctx, tx, userHK, true); err != nil {
		return Document{}, err
	}
	a, err := s.lock(ctx, tx, id)
	if err != nil || a.UserHK != userHK {
		return Document{}, notFound(err)
	}
	if a.Status != StatusPending {
		return Document{}, ErrNotPending
	}
	if len(a.Documents) >= MaxDocuments {
		return Document{}, ErrTooManyDocuments
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kyc_documents (id, application_id, type, file_size, mime_type, storage_path, content_digest)
		VALUES ($1, $2, $3::document_type, $4, $5, $6, $7)`, docID, id, string(docType), size, mime, key, digest); err != nil {
		return Document{}, fmt.Errorf("kyc: record the document: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.KYCDocuments, "id = $1", docID); err != nil {
		return Document{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	e := audit.Of(origin, audit.ActionUserKYCDocument, audit.TargetKYCApplication, id)
	e.Changes = &audit.Changes{After: map[string]any{"document": docID, "type": string(docType), "mime_type": mime, "size": size}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Document{}, err
	}
	after, err := s.read(ctx, tx, id)
	if err != nil {
		return Document{}, err
	}
	if err := tx.Commit(); err != nil {
		return Document{}, err
	}
	for _, d := range after.Documents {
		if d.ID == docID {
			return d, nil
		}
	}
	return Document{}, ErrNotFound
}

// Submit sends the user's pending application id for review, once it has
// a document of each required group.
func (s Service) Submit(ctx context.Context, userHK, id string, origin audit.Event) (Application, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Application{}, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockUser(ctx, tx, userHK, false); err != nil {
		return Application{}, err
	}
	a, err := s.lock(ctx, tx, id)
	if err != nil || a.UserHK != userHK {
		return Application{}, notFound(err)
	}
	if a.Provider != ProviderManual {
		return Application{}, ErrProviderFlow
	}
	if a.Status != StatusPending {
		return Application{}, ErrNotPending
	}
	if missing := missingDocuments(a.Documents); len(missing) > 0 {
		return Application{}, &MissingDocuments{Missing: missing}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kyc_applications SET status = 'UNDER_REVIEW', submitted_at = NOW() WHERE id = $1`, id); err != nil {
		return Application{}, fmt.Errorf("kyc: submit the application: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.KYCApplications, "id = $1", id); err != nil {
		return Application{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	e := audit.Of(origin, audit.ActionUserKYCSubmit, audit.TargetKYCApplication, id)
	e.Changes = &audit.Changes{After: map[string]any{"documents": len(a.Documents)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Application{}, err
	}
	out, err := s.read(ctx, tx, id)
	if err != nil {
		return Application{}, err
	}
	return out.Application, tx.Commit()
}

// missingDocuments returns the required groups that docs has no document
// of.
func missingDocuments(docs []Document) [][]DocumentType {
	var missing [][]DocumentType
	for _, group := range Required {
		if !slices.ContainsFunc(docs, func(d Document) bool { return slices.Contains(group, d.Type) }) {
			missing = append(missing, group)
		}
	}
	return missing
}

// Current returns the user's newest application, ErrNotFound when there
// is none.
func (s Service) Current(ctx context.Context, userHK string) (Application, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM kyc_applications WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT 1`, userHK).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, err
	}
	a, err := s.read(ctx, s.DB, id)
	if err != nil {
		return Application{}, err
	}
	a.UserHK = ""
	return a.Application, nil
}

// MaxList is the most applications a list returns.
const MaxList = 100

// Queue lists the applications of a status, the oldest submitted first,
// for the administrators; UNDER_REVIEW for "".
func (s Service) Queue(ctx context.Context, status Status, limit int) ([]Application, error) {
	if status == "" {
		status = StatusUnderReview
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM kyc_applications WHERE status = $1::kyc_status
		ORDER BY submitted_at NULLS LAST, created_at, id LIMIT $2`, string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("kyc: list the applications: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Application, 0, len(ids))
	for _, id := range ids {
		a, err := s.read(ctx, s.DB, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a.Application)
	}
	return out, nil
}

// Detail returns an application with its form opened, for the
// administrator of origin; the reading is audited.
func (s Service) Detail(ctx context.Context, id string, origin audit.Event) (Review, error) {
	c, err := s.available()
	if err != nil {
		return Review{}, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return Review{}, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Review{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := s.read(ctx, tx, id)
	if err != nil {
		return Review{}, notFound(err)
	}
	raw, err := c.Open(formPlace(id), a.form)
	if err != nil {
		return Review{}, fmt.Errorf("kyc: open the form of %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(raw), &a.Form); err != nil {
		return Review{}, fmt.Errorf("kyc: the form of %s: %w", id, err)
	}
	e := audit.Of(origin, audit.ActionAdminKYCView, audit.TargetKYCApplication, id)
	if err := audit.Record(ctx, tx, e); err != nil {
		return Review{}, err
	}
	return a.Review, tx.Commit()
}

// linkDomain is the domain of the blind index that signs the links.
const linkDomain = "kyc_document_link"

func linkMessage(docID, adminHK string, expires int64) string {
	return docID + "|" + adminHK + "|" + strconv.FormatInt(expires, 10)
}

// Link signs a link to the content of document docID for the
// administrator adminHK, which works for LinkTTL. The signature is a keyed
// hash of the data keys (#140), so every instance checks it, and it
// stays valid over a rotation of the keys.
func (s Service) Link(ctx context.Context, docID, adminHK string) (Link, error) {
	c, err := s.available()
	if err != nil {
		return Link{}, err
	}
	if _, err := s.document(ctx, docID); err != nil {
		return Link{}, err
	}
	ttl := s.LinkTTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	expires := s.now().Add(ttl).Unix()
	sig, err := c.Index(linkDomain, linkMessage(docID, adminHK, expires))
	if err != nil {
		return Link{}, err
	}
	q := url.Values{"admin": {adminHK}, "expires": {strconv.FormatInt(expires, 10)}, "sig": {base64.RawURLEncoding.EncodeToString(sig)}}
	return Link{URL: fmt.Sprintf(s.LinkPath, docID) + "?" + q.Encode(), ExpiresAt: time.Unix(expires, 0).UTC()}, nil
}

// Content returns the content of document docID and its kind, for a link
// that Link signed and that has not expired, and audits the reading as the
// administrator it was signed for. The content must be what was stored:
// its digest is under the row's seal, and its encryption binds it to the
// document.
func (s Service) Content(ctx context.Context, docID, adminHK, expires, sig string, origin audit.Event) ([]byte, string, error) {
	c, err := s.available()
	if err != nil {
		return nil, "", err
	}
	at, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || s.now().Unix() > at {
		return nil, "", ErrLinkInvalid
	}
	given, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, "", ErrLinkInvalid
	}
	valid := false
	for _, want := range c.Indexes(linkDomain, linkMessage(docID, adminHK, at)) {
		valid = valid || hmac.Equal(want, given)
	}
	if !valid {
		return nil, "", ErrLinkInvalid
	}
	d, err := s.document(ctx, docID)
	if err != nil {
		return nil, "", err
	}
	stored, err := s.Store.Get(ctx, d.key)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(stored)
	if !hmac.Equal(digest[:], d.digest) {
		log.Printf("SECURITY: the stored content of KYC document %s does not match its digest", docID)
		return nil, "", ErrContentChanged
	}
	content, err := c.Open(contentPlace(docID), stored)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrContentChanged, err)
	}
	origin.ActorType, origin.ActorID = audit.ActorAdmin, adminHK
	e := audit.Of(origin, audit.ActionAdminKYCDocumentView, audit.TargetKYCApplication, d.application)
	e.Changes = &audit.Changes{After: map[string]any{"document": docID, "type": string(d.Type)}}
	if err := audit.Record(ctx, s.DB, e); err != nil {
		return nil, "", err
	}
	return []byte(content), d.MimeType, nil
}

// Approve approves an application under review, and grants its level in
// the same transaction, with the user's row locked first. A user who lost
// level 1 since is not raised.
func (s Service) Approve(ctx context.Context, id, adminHK, notes string, origin audit.Event) (Application, error) {
	return s.review(ctx, id, adminHK, true, "", notes, origin, StatusUnderReview)
}

// Reject rejects an application under review with reason, which the user
// reads, 1 to 500 characters. The user may apply again.
func (s Service) Reject(ctx context.Context, id, adminHK, reason, notes string, origin audit.Event) (Application, error) {
	reason = strings.TrimSpace(reason)
	if n := len([]rune(reason)); n < 1 || n > 500 {
		return Application{}, Invalid{"reason": "must be 1 to 500 characters"}
	}
	return s.review(ctx, id, adminHK, false, reason, notes, origin, StatusUnderReview)
}

// review decides an application of one of the statuses from, as the
// administrator adminHK, or as the provider for "".
func (s Service) review(ctx context.Context, id, adminHK string, approve bool, reason, notes string, origin audit.Event,
	from ...Status) (Application, error) {
	if n := len([]rune(notes)); n > 1000 {
		return Application{}, Invalid{"notes": "must be at most 1000 characters"}
	}
	if _, err := uuid.Parse(id); err != nil {
		return Application{}, ErrNotFound
	}
	var userHK string
	if err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM kyc_applications WHERE id = $1`, id).Scan(&userHK); err != nil {
		return Application{}, notFound(err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Application{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockUser(ctx, tx, userHK, false); err != nil {
		return Application{}, err
	}
	a, err := s.lock(ctx, tx, id)
	if err != nil || a.UserHK != userHK {
		return Application{}, notFound(err)
	}
	if !slices.Contains(from, a.Status) {
		return Application{}, ErrNotUnderReview
	}
	status, action := StatusRejected, audit.ActionAdminKYCReject
	if adminHK == "" {
		action = audit.ActionKYCProviderVerdict
	}
	if approve {
		status = StatusApproved
		if adminHK != "" {
			action = audit.ActionAdminKYCApprove
		}
		st, err := verification.Store{}.Level(ctx, tx, userHK)
		if err != nil {
			return Application{}, err
		}
		if st.Level < verification.Level1 {
			return Application{}, ErrLevelRequired
		}
		if _, err := verification.SetLevel(ctx, tx, userHK, verification.Level2, "KYC application "+id, origin); err != nil {
			return Application{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kyc_applications SET status = $2::kyc_status, reviewed_at = NOW(), reviewed_by = NULLIF($3, '')::uuid, submitted_at = COALESCE(submitted_at, NOW()),
			reason = NULLIF($4, ''), review_notes = NULLIF($5, '') WHERE id = $1`, id, string(status), adminHK, reason, notes); err != nil {
		return Application{}, fmt.Errorf("kyc: review the application: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.KYCApplications, "id = $1", id); err != nil {
		return Application{}, err
	}
	e := audit.Of(origin, action, audit.TargetKYCApplication, id)
	after := map[string]any{"status": string(status), "user": userHK}
	if reason != "" {
		after["reason"] = reason
	}
	e.Changes = &audit.Changes{Before: map[string]any{"status": string(a.Status)}, After: after}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Application{}, err
	}
	out, err := s.read(ctx, tx, id)
	if err != nil {
		return Application{}, err
	}
	return out.Application, tx.Commit()
}

func notFound(err error) error {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Session returns a token of the provider's SDK for the user's open
// application, which a provider verifies.
func (s Service) Session(ctx context.Context, userHK string) (Session, error) {
	if s.Provider == nil {
		return Session{}, ErrProviderFlow
	}
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM kyc_applications WHERE user_id = $1 AND status IN ('PENDING', 'UNDER_REVIEW')`,
		userHK).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	a, err := s.read(ctx, s.DB, id)
	if err != nil {
		return Session{}, err
	}
	if a.Provider != s.Provider.Name() {
		return Session{}, ErrProviderFlow
	}
	session, err := s.Provider.Session(ctx, id)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return session, nil
}

// rejectedByProvider is what a user reads of a final rejection by the
// provider; its labels are for the administrators.
const rejectedByProvider = "The verification was not successful. Please contact support."

// Webhook applies a webhook of the provider, once its signature is
// verified: a submission moves the application under review, an approval
// grants level 2, a final rejection rejects it, and a rejection that the
// user may answer sends it back to pending. A webhook of an application
// that is decided, or that changes nothing, is accepted and ignored, as
// the provider sends again what it did not see acknowledged.
func (s Service) Webhook(ctx context.Context, header http.Header, body []byte, origin audit.Event) error {
	if s.Provider == nil {
		return ErrProviderFlow
	}
	v, err := s.Provider.Verify(header, body)
	if err != nil {
		return err
	}
	if v.Outcome == OutcomeNone {
		return nil
	}
	if _, err := uuid.Parse(v.Ref); err != nil {
		return nil
	}
	a, err := s.read(ctx, s.DB, v.Ref)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Provider != s.Provider.Name() || a.providerRef != v.ProviderRef {
		log.Printf("SECURITY: a %s webhook names application %s, which is not its applicant %s", s.Provider.Name(), v.Ref, v.ProviderRef)
		return nil
	}
	origin.ActorType, origin.ActorID = audit.ActorSystem, ""
	switch v.Outcome {
	case OutcomeApproved:
		_, err = s.review(ctx, a.ID, "", true, "", "", origin, StatusPending, StatusUnderReview)
	case OutcomeRejected:
		_, err = s.review(ctx, a.ID, "", false, rejectedByProvider, strings.Join(v.Labels, ", "), origin, StatusPending, StatusUnderReview)
	case OutcomePending:
		err = s.move(ctx, a, StatusPending, StatusUnderReview, v, origin)
	case OutcomeRetry:
		err = s.move(ctx, a, StatusUnderReview, StatusPending, v, origin)
	}
	switch {
	case errors.Is(err, ErrNotUnderReview), errors.Is(err, ErrNotPending):
		return nil
	case errors.Is(err, ErrLevelRequired), errors.Is(err, verification.ErrInactive):
		// The verdict cannot apply, as the user lost level 1 or the
		// account is not active; the provider would send it again
		// forever. The application waits for an administrator.
		log.Printf("A %s verdict on KYC application %s was not applied: %v", s.Provider.Name(), a.ID, err)
		return nil
	}
	return err
}

// move moves an application from one status to another on a webhook, with
// the user's row locked first.
func (s Service) move(ctx context.Context, a stored, from, to Status, v Verdict, origin audit.Event) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockUser(ctx, tx, a.UserHK, false); err != nil {
		return err
	}
	locked, err := s.lock(ctx, tx, a.ID)
	if err != nil {
		return err
	}
	if locked.Status != from {
		return ErrNotPending
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kyc_applications SET status = $2::kyc_status,
			submitted_at = CASE WHEN $2 = 'PENDING' THEN NULL ELSE NOW() END WHERE id = $1`, a.ID, string(to)); err != nil {
		return fmt.Errorf("kyc: move the application: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.KYCApplications, "id = $1", a.ID); err != nil {
		return err
	}
	e := audit.Of(origin, audit.ActionKYCProviderVerdict, audit.TargetKYCApplication, a.ID)
	after := map[string]any{"status": string(to)}
	if len(v.Labels) > 0 {
		after["labels"] = v.Labels
	}
	e.Changes = &audit.Changes{Before: map[string]any{"status": string(from)}, After: after}
	if err := audit.Record(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit()
}
