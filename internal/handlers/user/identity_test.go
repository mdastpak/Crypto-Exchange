package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/idcheck"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIdentity struct {
	st  idcheck.Status
	err error
	got [2]string
}

func (f *fakeIdentity) Status(context.Context, string) (idcheck.Status, error) { return f.st, f.err }

func (f *fakeIdentity) Submit(_ context.Context, _, nationalID, birthDate string, _ audit.Event) (idcheck.Status, error) {
	f.got = [2]string{nationalID, birthDate}
	return f.st, f.err
}

func serveIdentity(t *testing.T, h *UserHandler, method, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: uuid.NewString(), SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.GET("/identity", h.GetIdentity)
	r.POST("/identity", h.SubmitIdentity)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, "/identity", strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestIdentityRoutes(t *testing.T) {
	status, resp := serveIdentity(t, &UserHandler{}, http.MethodGet, "")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, codeIdentityUnavailable, resp["code"])

	fake := &fakeIdentity{st: idcheck.Status{State: idcheck.StateVerified, NationalID: "00******79"}}
	h := &UserHandler{Identity: fake}
	const body = `{"national_id": " 0012345679 ", "birth_date": "1991-08-16"}`
	status, resp = serveIdentity(t, h, http.MethodPost, body)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "verified", resp["info"].(map[string]any)["state"])
	assert.Equal(t, [2]string{"0012345679", "1991-08-16"}, fake.got, "trimmed")

	fake.st.State = idcheck.StatePending
	status, _ = serveIdentity(t, h, http.MethodPost, body)
	assert.Equal(t, http.StatusAccepted, status)

	status, _ = serveIdentity(t, h, http.MethodPost, `{"national_id": "0012345679"}`)
	assert.Equal(t, http.StatusBadRequest, status)

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{idcheck.Invalid{"birth_date": "is not a date"}, http.StatusBadRequest, "validation_failed"},
		{idcheck.ErrNationalIDTaken, http.StatusConflict, codeNationalIDTaken},
		{idcheck.ErrAlreadyVerified, http.StatusConflict, codeIdentityVerified},
		{idcheck.ErrPending, http.StatusConflict, codeIdentityPending},
		{idcheck.ErrTooManyAttempts, http.StatusTooManyRequests, codeIdentityTooManyAttempts},
		{idcheck.ErrUnavailable, http.StatusServiceUnavailable, codeIdentityUnavailable},
		{rowseal.ErrUnverified, http.StatusConflict, "row_unverified"},
	} {
		fake.err = tc.err
		status, resp := serveIdentity(t, h, http.MethodPost, body)
		assert.Equal(t, tc.status, status, tc.err.Error())
		assert.Equal(t, tc.code, resp["code"], tc.err.Error())
		assert.NotContains(t, resp["message"], "0012345679")
	}
}
