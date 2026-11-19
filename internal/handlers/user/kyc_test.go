package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/kyc"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// fakeKYC answers with err, and keeps what it was given.
type fakeKYC struct {
	err     error
	docType kyc.DocumentType
	content []byte
}

func (f *fakeKYC) Current(context.Context, string) (kyc.Application, error) {
	return kyc.Application{ID: "a1", Status: kyc.StatusPending, Documents: []kyc.Document{}}, f.err
}

func (f *fakeKYC) Apply(context.Context, string, kyc.Form, audit.Event) (kyc.Application, error) {
	return kyc.Application{ID: "a1", Status: kyc.StatusPending, Documents: []kyc.Document{}}, f.err
}

func (f *fakeKYC) Upload(_ context.Context, _, _ string, docType kyc.DocumentType, content []byte, _ audit.Event) (kyc.Document, error) {
	f.docType, f.content = docType, content
	return kyc.Document{ID: "d1", Type: docType}, f.err
}

func (f *fakeKYC) Session(context.Context, string) (kyc.Session, error) {
	return kyc.Session{Provider: kyc.ProviderSumsub, Token: "token"}, f.err
}

func (f *fakeKYC) Webhook(context.Context, http.Header, []byte, audit.Event) error { return f.err }

func (f *fakeKYC) Submit(context.Context, string, string, audit.Event) (kyc.Application, error) {
	return kyc.Application{ID: "a1", Status: kyc.StatusUnderReview, Documents: []kyc.Document{}}, f.err
}

func serveKYC(t *testing.T, h *UserHandler, req *http.Request) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.GET("/kyc", h.GetKYC)
	r.POST("/kyc/applications", h.ApplyKYC)
	r.POST("/kyc/applications/:id/documents", h.UploadKYCDocument)
	r.POST("/kyc/applications/:id/submit", h.SubmitKYC)
	r.GET("/kyc/session", h.GetKYCSession)
	r.POST("/kyc/webhooks/sumsub", h.KYCWebhook)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func multipartUpload(t *testing.T, docType string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("type", docType))
	if content != nil {
		part, err := w.CreateFormFile("file", "scan.jpg")
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	req := httptest.NewRequest(http.MethodPost, "/kyc/applications/a1/documents", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestKYCRoutes_RunWithoutInfrastructure(t *testing.T) {
	fake := &fakeKYC{}
	h := &UserHandler{KYC: fake}

	status, _ := serveKYC(t, h, httptest.NewRequest(http.MethodGet, "/kyc", nil))
	assert.Equal(t, http.StatusOK, status)
	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/applications", bytes.NewBufferString(`{"form": {}, "extra": 1}`)))
	assert.Equal(t, http.StatusBadRequest, status, "an unknown field")
	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/applications", bytes.NewBufferString(`{"form": {"city": "Tehran"}}`)))
	assert.Equal(t, http.StatusCreated, status)

	status, _ = serveKYC(t, h, multipartUpload(t, "SELFIE", []byte{0xff, 0xd8, 0xff}))
	assert.Equal(t, http.StatusCreated, status)
	assert.Equal(t, kyc.DocSelfie, fake.docType)
	assert.Equal(t, []byte{0xff, 0xd8, 0xff}, fake.content)
	status, resp := serveKYC(t, h, multipartUpload(t, "SELFIE", nil))
	assert.Equal(t, http.StatusBadRequest, status, "a file is required")
	assert.Equal(t, "validation_failed", resp["code"])

	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/applications/a1/submit", nil))
	assert.Equal(t, http.StatusOK, status)

	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{kyc.ErrNotFound, http.StatusNotFound, codeKYCNotFound},
		{kyc.ErrLevelRequired, http.StatusForbidden, codeKYCLevelRequired},
		{kyc.ErrAlreadyVerified, http.StatusConflict, codeKYCAlreadyVerified},
		{kyc.ErrOpenApplication, http.StatusConflict, codeKYCOpenApplication},
		{kyc.ErrNotPending, http.StatusConflict, codeKYCNotPending},
		{kyc.ErrTooManyDocuments, http.StatusConflict, codeKYCTooManyDocuments},
		{kyc.ErrUnavailable, http.StatusServiceUnavailable, codeKYCUnavailable},
		{rowseal.ErrUnverified, http.StatusConflict, "row_unverified"},
		{kyc.Invalid{"city": "x"}, http.StatusBadRequest, "validation_failed"},
		{&kyc.MissingDocuments{Missing: [][]kyc.DocumentType{{kyc.DocSelfie}}}, http.StatusConflict, codeKYCDocumentsIncomplete},
	} {
		fake.err = c.err
		status, resp := serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/applications/a1/submit", nil))
		assert.Equal(t, c.status, status, c.err.Error())
		assert.Equal(t, c.code, resp["code"], c.err.Error())
	}

	fake.err = nil
	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodGet, "/kyc/session", nil))
	assert.Equal(t, http.StatusOK, status)
	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/webhooks/sumsub", bytes.NewBufferString(`{}`)))
	assert.Equal(t, http.StatusOK, status)
	fake.err = kyc.ErrWebhookSignature
	status, resp = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/webhooks/sumsub", bytes.NewBufferString(`{}`)))
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, codeKYCWebhookUnsigned, resp["code"])
	fake.err = errors.New("database down")
	status, _ = serveKYC(t, h, httptest.NewRequest(http.MethodPost, "/kyc/webhooks/sumsub", bytes.NewBufferString(`{}`)))
	assert.Equal(t, http.StatusInternalServerError, status, "the provider sends it again")
	fake.err = kyc.ErrProviderFlow
	status, resp = serveKYC(t, h, httptest.NewRequest(http.MethodGet, "/kyc/session", nil))
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, codeKYCProviderFlow, resp["code"])

	status, resp = serveKYC(t, &UserHandler{}, httptest.NewRequest(http.MethodGet, "/kyc", nil))
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeKYCUnavailable, resp["code"])
}
