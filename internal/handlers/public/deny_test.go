package public

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/denylink"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDenier struct {
	out    denylink.Outcome
	err    error
	tokens []string
	origin audit.Event
}

func (f *fakeDenier) Deny(_ context.Context, token string, origin audit.Event) (denylink.Outcome, error) {
	f.tokens = append(f.tokens, token)
	f.origin = origin
	return f.out, f.err
}

func serveDeny(t *testing.T, h *PublicHandler, method string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/deny/:token", h.Deny)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/deny/Ab3xK9q2LmPq", nil)
	req.RemoteAddr = "203.0.113.7:4000"
	r.ServeHTTP(w, req)
	var resp map[string]any
	if strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	}
	return w, resp
}

// TestDeny_AnswersWhatTheLinkDid runs the route of the "this was not me"
// links (#374) on a fake: what the link did, the same answer for every
// link that is not usable, and nothing on a GET.
func TestDeny_AnswersWhatTheLinkDid(t *testing.T) {
	d := &fakeDenier{out: denylink.Outcome{WithdrawalsCancelled: 1, SessionsEnded: 2}}
	h := &PublicHandler{Denials: d}
	w, resp := serveDeny(t, h, http.MethodPost)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	info, ok := resp["info"].(map[string]any)
	require.True(t, ok, w.Body.String())
	assert.EqualValues(t, 1, info["withdrawals_cancelled"])
	assert.EqualValues(t, 2, info["sessions_ended"])
	assert.Equal(t, []string{"Ab3xK9q2LmPq"}, d.tokens)
	assert.Equal(t, "203.0.113.7", d.origin.IP, "the request is audited")

	w, _ = serveDeny(t, h, http.MethodGet)
	assert.Equal(t, http.StatusNotFound, w.Code, "opening the link changes nothing")
	assert.Len(t, d.tokens, 1)

	d.err = denylink.ErrInvalid
	w, resp = serveDeny(t, h, http.MethodPost)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, codeDenyLinkInvalid, resp["code"])
	d.err = errors.New("the database is down")
	w, _ = serveDeny(t, h, http.MethodPost)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	w, resp = serveDeny(t, &PublicHandler{}, http.MethodPost)
	assert.Equal(t, http.StatusNotFound, w.Code, "without the links, none is usable")
	assert.Equal(t, codeDenyLinkInvalid, resp["code"])
}
