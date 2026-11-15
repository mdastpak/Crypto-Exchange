package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rebrand"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeConversions struct {
	err      error
	accounts []ledger.Account
	sources  []string
}

func (f *fakeConversions) ConvertOwn(_ context.Context, _, source string, a ledger.Account) (rebrand.Converted, error) {
	f.accounts, f.sources = append(f.accounts, a), append(f.sources, source)
	if f.err != nil {
		return rebrand.Converted{}, f.err
	}
	return rebrand.Converted{EntryID: "e1", Source: source, Amount: "2.5", Credited: "60000", Dust: "0"}, nil
}

func serveConversion(t *testing.T, h *UserHandler, user, migration, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		auth.SetClaims(c, &auth.Claims{UUID: user, SessionID: uuid.NewString(), Role: auth.RoleUser})
	})
	r.POST("/currency-migrations/:id/convert", h.ConvertMigrationHolding)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/currency-migrations/"+migration+"/convert", strings.NewReader(body)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

func TestConversionRoute(t *testing.T) {
	user, migration, source := uuid.NewString(), uuid.NewString(), uuid.NewString()
	body := `{"source_hk": "` + source + `"}`
	status, resp := serveConversion(t, &UserHandler{}, user, migration, body)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "conversions_unavailable", resp["code"])

	service := &fakeConversions{}
	h := &UserHandler{Conversions: service}
	status, resp = serveConversion(t, h, user, migration, body)
	require.Equal(t, http.StatusCreated, status, resp)
	assert.Equal(t, "60000", resp["info"].(map[string]any)["credited"])
	require.Len(t, service.accounts, 1)
	assert.Equal(t, user, service.accounts[0].UserHK.String(), "the holding of the signed-in user")
	assert.Nil(t, service.accounts[0].SubAccountID)
	assert.Equal(t, source, service.sources[0])

	status, resp = serveConversion(t, h, user, "nope", `{"source_hk": "x"}`)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, resp["info"].(map[string]any)["errors"], "source_hk")

	for err, want := range map[error]struct {
		status int
		code   string
	}{
		rebrand.ErrNotFound:           {http.StatusNotFound, "migration_not_found"},
		rebrand.ErrNotASource:         {http.StatusNotFound, "not_a_source"},
		rebrand.ErrNotConverting:      {http.StatusConflict, "migration_not_converting"},
		rebrand.ErrNothingToConvert:   {http.StatusConflict, "nothing_to_convert"},
		ledger.ErrInsufficientBalance: {http.StatusConflict, "insufficient_balance"},
	} {
		service.err = err
		status, resp = serveConversion(t, h, user, migration, body)
		assert.Equal(t, want.status, status, err)
		assert.Equal(t, want.code, resp["code"], err)
	}
}
