package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/kyc"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// kycJPEG is the start of a JPEG, as its content shows it.
var kycJPEG = append([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, bytes.Repeat([]byte{9}, 64)...)

// upload posts a document as multipart/form-data.
func (e *e2eEnv) upload(t *testing.T, id, docType string, content []byte, token string) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("type", docType))
	part, err := w.CreateFormFile("file", "my passport.jpg")
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/kyc/applications/"+id+"/documents", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestKYC walks through #72 on the routes: a user at level 1 applies for
// level 2 with a form and documents and submits it, an administrator
// reads it, opens a document through its signed link, and approves it,
// which raises the user to level 2.
func TestKYC(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	require.NotNil(t, env.handlers.DataKeys)
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))
	store := &kyc.Memory{}
	service := kyc.Service{DB: env.db, Store: store, LinkPath: kycDocumentPath}
	env.handlers.User.KYC, env.handlers.Admin.KYC = service, service

	token, userID := env.signedInUser(t, "kyc@example.com")
	status, raw := env.call(t, http.MethodPost, "/api/v1/user/kyc/applications", map[string]any{"form": kyc.Form{
		Address: "No. 12, Vali Asr St.", City: "Tehran", PostalCode: "1234567890", Occupation: "Engineer", SourceOfFunds: "Salary"}}, token)
	require.Equal(t, http.StatusForbidden, status, string(raw))
	assert.Contains(t, string(raw), "identity_verification_required", "level 1 first")

	tx, err := env.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = verification.SetLevel(ctx, tx, userID, verification.Level1, "identity verified", audit.Event{ActorType: audit.ActorSystem})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	status, raw = env.call(t, http.MethodPost, "/api/v1/user/kyc/applications", map[string]any{"form": kyc.Form{
		Address: "No. 12, Vali Asr St.", City: "Tehran", PostalCode: "1234567890", Occupation: "Engineer", SourceOfFunds: "Salary"}}, token)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var opened struct {
		Info kyc.Application `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &opened))
	id := opened.Info.ID

	status, raw = env.call(t, http.MethodPost, "/api/v1/user/kyc/applications/"+id+"/submit", nil, token)
	require.Equal(t, http.StatusConflict, status, string(raw))
	assert.Contains(t, string(raw), "kyc_documents_missing")

	status, raw = env.upload(t, id, "SELFIE", []byte("not an image"), token)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	var docID string
	for _, d := range []string{"NATIONAL_ID", "SELFIE", "PROOF_OF_ADDRESS"} {
		status, raw = env.upload(t, id, d, kycJPEG, token)
		require.Equal(t, http.StatusCreated, status, string(raw))
		if docID == "" {
			var added struct {
				Info kyc.Document `json:"info"`
			}
			require.NoError(t, json.Unmarshal(raw, &added))
			docID = added.Info.ID
		}
	}
	status, raw = env.call(t, http.MethodPost, "/api/v1/user/kyc/applications/"+id+"/submit", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodGet, "/api/v1/user/kyc", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), `"UNDER_REVIEW"`)
	assert.NotContains(t, string(raw), "my passport", "the name of the file is not kept")

	enrollment, err := env.accounts.Create(ctx, "kyc-reviewer@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/kyc/applications", nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), id)
	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/kyc/applications/"+id, nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "Vali Asr", "the form, opened for the reviewer")

	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/kyc/documents/"+docID+"/link", nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	var linked struct {
		Info kyc.Link `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &linked))
	status, content := env.call(t, http.MethodGet, linked.Info.URL, nil, "")
	require.Equal(t, http.StatusOK, status, string(content))
	assert.Equal(t, kycJPEG, content, "opened without the token, by its signature")
	status, raw = env.call(t, http.MethodGet, linked.Info.URL+"0", nil, "")
	assert.Equal(t, http.StatusForbidden, status, string(raw))

	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/kyc/applications/"+id+"/approve",
		map[string]string{"notes": "Documents match."}, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	st, err := verification.Store{}.Level(ctx, env.db, userID)
	require.NoError(t, err)
	assert.Equal(t, verification.Level2, st.Level)
}

// TestKYCProvider walks through the provider flow of #72 on the routes: an
// application of a level-1 user is opened at Sumsub, the user gets a
// token of its SDK, and Sumsub's signed webhooks approve it.
func TestKYCProvider(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))
	sumsub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/resources/applicants":
			_, _ = w.Write([]byte(`{"id":"applicant-1"}`))
		case "/resources/accessTokens":
			_, _ = w.Write([]byte(`{"token":"_act-sdk","userId":"` + r.URL.Query().Get("userId") + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(sumsub.Close)
	const secret = "webhook-secret"
	service := kyc.Service{DB: env.db, Store: &kyc.Memory{}, LinkPath: kycDocumentPath,
		Provider: &kyc.Sumsub{BaseURL: sumsub.URL, AppToken: "app", SecretKey: "key", WebhookSecret: secret, Level: "basic-kyc-level",
			Client: sumsub.Client()}}
	env.handlers.User.KYC, env.handlers.Admin.KYC = service, service

	token, userID := env.signedInUser(t, "kyc-provider@example.com")
	tx, err := env.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = verification.SetLevel(ctx, tx, userID, verification.Level1, "identity verified", audit.Event{ActorType: audit.ActorSystem})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	status, raw := env.call(t, http.MethodPost, "/api/v1/user/kyc/applications", map[string]any{"form": kyc.Form{
		Address: "1 Main Street", City: "Dubai", PostalCode: "00000", Occupation: "Trader", SourceOfFunds: "Business"}}, token)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var opened struct {
		Info kyc.Application `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &opened))
	assert.Equal(t, kyc.ProviderSumsub, opened.Info.Provider)

	status, raw = env.upload(t, opened.Info.ID, "SELFIE", kycJPEG, token)
	require.Equal(t, http.StatusConflict, status, string(raw))
	assert.Contains(t, string(raw), "kyc_provider_flow")
	status, raw = env.call(t, http.MethodGet, "/api/v1/user/kyc/session", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "_act-sdk")

	webhook := func(body string, sign bool) (int, []byte) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/kyc/webhooks/sumsub", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		if sign {
			req.Header.Set("X-Payload-Digest", hex.EncodeToString(mac.Sum(nil)))
		} else {
			req.Header.Set("X-Payload-Digest", "00")
		}
		rec := httptest.NewRecorder()
		env.router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	approved := `{"applicantId":"applicant-1","externalUserId":"` + opened.Info.ID +
		`","type":"applicantReviewed","reviewResult":{"reviewAnswer":"GREEN"}}`
	status, raw = webhook(approved, false)
	require.Equal(t, http.StatusUnauthorized, status, string(raw))
	assert.Contains(t, string(raw), "kyc_webhook_unsigned")
	status, raw = webhook(approved, true)
	require.Equal(t, http.StatusOK, status, string(raw))
	st, err := verification.Store{}.Level(ctx, env.db, userID)
	require.NoError(t, err)
	assert.Equal(t, verification.Level2, st.Level)
	status, raw = webhook(approved, true)
	assert.Equal(t, http.StatusOK, status, "a repeated webhook is acknowledged: %s", raw)
}
