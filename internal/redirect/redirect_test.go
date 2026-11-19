package redirect

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefuse_FollowsNoRedirect(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer origin.Close()

	client := origin.Client()
	client.CheckRedirect = Refuse
	resp, err := client.Get(origin.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRefused))
	assert.False(t, reached)
}

func TestHTTPSOnly(t *testing.T) {
	var plainReached, tlsReached bool
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainReached = true }))
	defer plain.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { tlsReached = true }))
	defer other.Close()

	toPlain := httptest.NewTLSServer(http.RedirectHandler(plain.URL, http.StatusFound))
	defer toPlain.Close()
	client := toPlain.Client()
	client.CheckRedirect = HTTPSOnly
	resp, err := client.Get(toPlain.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.ErrorIs(t, err, ErrRefused)
	assert.False(t, plainReached, "a redirect to plain http is refused")

	// Both TLS test servers trust the same certificate, so one client can
	// reach both: a redirect to another https host is followed.
	toOther := httptest.NewTLSServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer toOther.Close()
	client = toOther.Client()
	client.CheckRedirect = HTTPSOnly
	resp, err = client.Get(toOther.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.True(t, tlsReached)
}

func TestHTTPSOnly_StopsALoop(t *testing.T) {
	var loop *httptest.Server
	loop = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loop.URL, http.StatusFound)
	}))
	defer loop.Close()
	client := loop.Client()
	client.CheckRedirect = HTTPSOnly
	resp, err := client.Get(loop.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.ErrorIs(t, err, ErrRefused)
}
