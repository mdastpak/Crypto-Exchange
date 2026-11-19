package kyc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/usertest"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// jpeg and pdf are the starts of a JPEG and a PDF, as their content shows
// them.
var (
	jpeg = append([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte{7}, 64)...)
	pdf  = []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n")
)

var form = Form{Address: "No. 12, Vali Asr St.", City: "Tehran", PostalCode: "1234567890", Occupation: "Engineer",
	SourceOfFunds: "Salary"}

type fixture struct {
	Service
	t     *testing.T
	store *Memory
	user  string
	admin string
}

// newFixture turns on row seals in enforce mode and field encryption, with
// data keys of a new local KEK, and gives a user at level 1.
func newFixture(t *testing.T) fixture {
	t.Helper()
	conn := pgtest.Migrated(t, "testdb_kyc")
	kek := make([]byte, 32)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: conn, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	sealer := rowseal.New(conn, live, rowseal.ModeEnforce)
	sealer.Logger = log.New(io.Discard, "", 0)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	t.Cleanup(field.Use(field.New(live)))

	store := &Memory{}
	f := fixture{Service: Service{DB: conn, Store: store, LinkPath: "/api/v1/admin/kyc/documents/%s/content"},
		t: t, store: store}
	f.user = f.newUser("kyc@example.com", verification.Level1)
	f.admin = f.newUser("reviewer@example.com", verification.Level0)
	return f
}

func (f fixture) newUser(email string, level verification.Level) string {
	f.t.Helper()
	ctx := context.Background()
	hk := usertest.Insert(f.t, f.DB, usertest.User{Email: email})
	require.NoError(f.t, rowseal.Reseal(ctx, f.DB, rowseal.Users, "hk = $1", hk))
	if level > verification.Level0 {
		tx, err := f.DB.BeginTx(ctx, nil)
		require.NoError(f.t, err)
		_, err = verification.SetLevel(ctx, tx, hk, level, "test", audit.Event{ActorType: audit.ActorSystem})
		require.NoError(f.t, err)
		require.NoError(f.t, tx.Commit())
	}
	return hk
}

func (f fixture) level(user string) verification.Level {
	f.t.Helper()
	st, err := verification.Store{}.Level(context.Background(), f.DB, user)
	require.NoError(f.t, err)
	return st.Level
}

func (f fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.DB.QueryRow(query, args...).Scan(&n))
	return n
}

var userOrigin = audit.Event{IP: "203.0.113.4"}

func (f fixture) adminOrigin() audit.Event {
	return audit.Event{ActorType: audit.ActorAdmin, ActorID: f.admin, IP: "198.51.100.2"}
}

// complete opens an application with the three required documents.
func (f fixture) complete() Application {
	f.t.Helper()
	ctx := context.Background()
	a, err := f.Apply(ctx, f.user, form, userOrigin)
	require.NoError(f.t, err)
	for _, d := range []struct {
		t       DocumentType
		content []byte
	}{{DocNationalID, jpeg}, {DocSelfie, jpeg}, {DocUtilityBill, pdf}} {
		_, err := f.Upload(ctx, f.user, a.ID, d.t, d.content, userOrigin)
		require.NoError(f.t, err)
	}
	return a
}

// An application goes from pending to under review to approved, which
// raises the user to level 2 in the same transaction; personal data is
// stored encrypted only (#72).
func TestApplication_ApprovedGrantsLevel2(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a := f.complete()
	assert.Equal(t, StatusPending, a.Status)

	var sealed []byte
	require.NoError(t, f.DB.QueryRow(`SELECT form_data_enc FROM kyc_applications WHERE id = $1`, a.ID).Scan(&sealed))
	assert.NotContains(t, string(sealed), "Vali Asr", "the form is encrypted")
	for _, d := range []string{"Vali Asr", "Tehran", "1234567890"} {
		assert.Zero(t, f.count(`SELECT COUNT(*) FROM audit_events WHERE changes::text LIKE '%' || $1 || '%'`, d), "no personal data in the audit log")
	}
	assert.Equal(t, 3, f.store.Len())
	for key := range f.store.objects {
		assert.False(t, bytes.Contains(f.store.objects[key], jpeg[4:20]) || bytes.Contains(f.store.objects[key], []byte("%PDF")),
			"the stored objects are encrypted")
	}

	submitted, err := f.Submit(ctx, f.user, a.ID, userOrigin)
	require.NoError(t, err)
	assert.Equal(t, StatusUnderReview, submitted.Status)
	require.NotNil(t, submitted.SubmittedAt)

	queue, err := f.Queue(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, f.user, queue[0].UserHK)

	review, err := f.Detail(ctx, a.ID, f.adminOrigin())
	require.NoError(t, err)
	assert.Equal(t, form, review.Form)
	assert.Len(t, review.Documents, 3)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'admin.kyc_view'`), "reading the personal data is audited")

	approved, err := f.Approve(ctx, a.ID, f.admin, "documents match", f.adminOrigin())
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, approved.Status)
	assert.Equal(t, verification.Level2, f.level(f.user))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = $1`, audit.ActionAdminKYCApprove))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = $1 AND actor_id = $2`, audit.ActionUserVerificationSet, f.admin),
		"the level is granted by the reviewer")

	_, err = f.Approve(ctx, a.ID, f.admin, "", f.adminOrigin())
	assert.ErrorIs(t, err, ErrNotUnderReview, "once")
	_, err = f.Apply(ctx, f.user, form, userOrigin)
	assert.ErrorIs(t, err, ErrAlreadyVerified)
}

// Every transition that the statuses do not allow is refused (#72).
func TestApplication_Transitions(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	newcomer := f.newUser("new@example.com", verification.Level0)
	_, err := f.Apply(ctx, newcomer, form, userOrigin)
	assert.ErrorIs(t, err, ErrLevelRequired, "the identity first")

	_, err = f.Apply(ctx, f.user, Form{Address: "x"}, userOrigin)
	var invalid Invalid
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid, "city")

	a, err := f.Apply(ctx, f.user, form, userOrigin)
	require.NoError(t, err)
	_, err = f.Apply(ctx, f.user, form, userOrigin)
	assert.ErrorIs(t, err, ErrOpenApplication, "one open application")

	_, err = f.Submit(ctx, f.user, a.ID, userOrigin)
	var missing *MissingDocuments
	require.ErrorAs(t, err, &missing)
	assert.Len(t, missing.Missing, 3)

	_, err = f.Upload(ctx, f.user, a.ID, DocSelfie, []byte("plain text, not an image"), userOrigin)
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid, "file")
	_, err = f.Upload(ctx, f.user, a.ID, "HOROSCOPE", jpeg, userOrigin)
	require.ErrorAs(t, err, &invalid)
	other := f.newUser("other@example.com", verification.Level1)
	_, err = f.Upload(ctx, other, a.ID, DocSelfie, jpeg, userOrigin)
	assert.ErrorIs(t, err, ErrNotFound, "not another user's")
	assert.Zero(t, f.store.Len(), "nothing stored for a refused upload")

	_, err = f.Approve(ctx, a.ID, f.admin, "", f.adminOrigin())
	assert.ErrorIs(t, err, ErrNotUnderReview, "a pending application is not reviewed")

	for _, d := range []DocumentType{DocPassport, DocSelfie, DocBankStatement} {
		_, err := f.Upload(ctx, f.user, a.ID, d, jpeg, userOrigin)
		require.NoError(t, err)
	}
	_, err = f.Submit(ctx, f.user, a.ID, userOrigin)
	require.NoError(t, err)
	_, err = f.Upload(ctx, f.user, a.ID, DocSelfie, jpeg, userOrigin)
	assert.ErrorIs(t, err, ErrNotPending, "a submitted application takes no document")
	_, err = f.Submit(ctx, f.user, a.ID, userOrigin)
	assert.ErrorIs(t, err, ErrNotPending)

	_, err = f.Reject(ctx, a.ID, f.admin, "", "", f.adminOrigin())
	require.ErrorAs(t, err, &invalid, "a rejection says why")
	rejected, err := f.Reject(ctx, a.ID, f.admin, "The selfie is blurred.", "", f.adminOrigin())
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, rejected.Status)
	assert.Equal(t, "The selfie is blurred.", rejected.Reason)
	assert.Equal(t, verification.Level1, f.level(f.user), "a rejection grants nothing")
	_, err = f.Approve(ctx, a.ID, f.admin, "", f.adminOrigin())
	assert.ErrorIs(t, err, ErrNotUnderReview, "a rejection is final")

	current, err := f.Current(ctx, f.user)
	require.NoError(t, err)
	assert.Equal(t, a.ID, current.ID)
	assert.Empty(t, current.UserHK)
	_, err = f.Apply(ctx, f.user, form, userOrigin)
	require.NoError(t, err, "a rejected user applies again")

	// A user who lost level 1 while the application waited is not raised.
	b := f.complete2(other)
	tx, err := f.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = verification.SetLevel(ctx, tx, other, verification.Level0, "revoked", audit.Event{ActorType: audit.ActorSystem})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = f.Approve(ctx, b.ID, f.admin, "", f.adminOrigin())
	assert.ErrorIs(t, err, ErrLevelRequired)
}

// complete2 opens and submits a complete application of user.
func (f fixture) complete2(user string) Application {
	f.t.Helper()
	ctx := context.Background()
	a, err := f.Apply(ctx, user, form, userOrigin)
	require.NoError(f.t, err)
	for _, d := range []DocumentType{DocNationalID, DocSelfie, DocProofOfAddress} {
		_, err := f.Upload(ctx, user, a.ID, d, jpeg, userOrigin)
		require.NoError(f.t, err)
	}
	a, err = f.Submit(ctx, user, a.ID, userOrigin)
	require.NoError(f.t, err)
	return a
}

// A document is read through a link signed for one administrator, for a
// minute; its content must be what was stored (#72).
func TestDocumentLink(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a := f.complete()
	app, err := f.read(ctx, f.DB, a.ID)
	require.NoError(t, err)
	doc := app.Documents
	require.Len(t, doc, 3)
	id := doc[0].ID

	now := time.Now()
	f.Now = func() time.Time { return now }
	link, err := f.Link(ctx, id, f.admin)
	require.NoError(t, err)
	u, err := url.Parse(link.URL)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(u.Path, "/api/v1/admin/kyc/documents/"+id))
	q := u.Query()

	content, mime, err := f.Content(ctx, id, q.Get("admin"), q.Get("expires"), q.Get("sig"), audit.Event{IP: "198.51.100.2"})
	require.NoError(t, err)
	assert.Equal(t, jpeg, content)
	assert.Equal(t, "image/jpeg", mime)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM audit_events WHERE action = 'admin.kyc_document_view' AND actor_id = $1`, f.admin))

	_, _, err = f.Content(ctx, id, f.user, q.Get("expires"), q.Get("sig"), audit.Event{})
	assert.ErrorIs(t, err, ErrLinkInvalid, "signed for another administrator")
	_, _, err = f.Content(ctx, doc[1].ID, q.Get("admin"), q.Get("expires"), q.Get("sig"), audit.Event{})
	assert.ErrorIs(t, err, ErrLinkInvalid, "signed for another document")
	f.Now = func() time.Time { return now.Add(2 * time.Minute) }
	_, _, err = f.Content(ctx, id, q.Get("admin"), q.Get("expires"), q.Get("sig"), audit.Event{})
	assert.ErrorIs(t, err, ErrLinkInvalid, "expired")
	f.Now = func() time.Time { return now }

	// An object swapped in the storage is refused, even encrypted by the
	// same keys for another document.
	var key, otherKey string
	require.NoError(t, f.DB.QueryRow(`SELECT storage_path FROM kyc_documents WHERE id = $1`, id).Scan(&key))
	require.NoError(t, f.DB.QueryRow(`SELECT storage_path FROM kyc_documents WHERE id = $1`, doc[2].ID).Scan(&otherKey))
	swapped, err := f.store.Get(ctx, otherKey)
	require.NoError(t, err)
	f.store.Replace(key, swapped)
	_, _, err = f.Content(ctx, id, q.Get("admin"), q.Get("expires"), q.Get("sig"), audit.Event{})
	assert.ErrorIs(t, err, ErrContentChanged)

	// A document moved, or its digest changed, in SQL is refused.
	_, err = f.DB.Exec(`UPDATE kyc_documents SET content_digest = sha256(content_digest) WHERE id = $1`, id)
	require.NoError(t, err)
	_, _, err = f.Content(ctx, id, q.Get("admin"), q.Get("expires"), q.Get("sig"), audit.Event{})
	assert.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'kyc_documents'`))
}

// An application approved in SQL grants nothing: it fails its seal (#72).
func TestApplication_ChangedInSQL_Refused(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	a := f.complete()
	_, err := f.DB.Exec(`UPDATE kyc_applications SET status = 'UNDER_REVIEW', submitted_at = NOW() WHERE id = $1`, a.ID)
	require.NoError(t, err)
	_, err = f.Approve(ctx, a.ID, f.admin, "", f.adminOrigin())
	require.ErrorIs(t, err, rowseal.ErrUnverified)
	assert.Equal(t, verification.Level1, f.level(f.user))
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'kyc_applications'`))
}

// Without data keys, nothing personal is taken (#72).
func TestUnavailableWithoutKeys(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(field.Use(nil))
	_, err := f.Apply(context.Background(), f.user, form, userOrigin)
	assert.ErrorIs(t, err, ErrUnavailable)
	_, err = Service{DB: f.DB}.Detail(context.Background(), "00000000-0000-0000-0000-000000000001", f.adminOrigin())
	assert.ErrorIs(t, err, ErrUnavailable, "nor without storage")
}
