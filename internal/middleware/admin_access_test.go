package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAdminAccess_ReadsRangesAndAddresses(t *testing.T) {
	access, err := ParseAdminAccess(false, []string{" 10.0.0.0/8 ", "192.0.2.7", "2001:db8::/32", ""})
	require.NoError(t, err)
	assert.True(t, access.Restricted())

	for ip, want := range map[string]bool{
		"10.1.2.3":         true,
		"192.0.2.7":        true,
		"192.0.2.8":        false,
		"::ffff:10.1.2.3":  true,
		"2001:db8::1":      true,
		"2001:db9::1":      false,
		"203.0.113.1":      false,
		"not an address":   false,
		"":                 false,
		"10.1.2.3:443":     false,
		"::ffff:192.0.2.8": false,
	} {
		assert.Equal(t, want, access.allows(ip), ip)
	}

	_, err = ParseAdminAccess(false, []string{"10.0.0.0/8", "office"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"office"`)
}

func TestParseAdminAccess_AnEmptyListAllowsEveryAddress(t *testing.T) {
	access, err := ParseAdminAccess(false, nil)
	require.NoError(t, err)
	assert.False(t, access.Restricted())
	assert.True(t, access.allows("203.0.113.1"))
}

// adminAccessRouter serves one admin route behind the access.
func adminAccessRouter(access AdminAccess) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin/thing", access.Middleware(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return r
}

func TestAdminAccess_RefusesAnAddressOutsideTheAllowedNetworks(t *testing.T) {
	access, err := ParseAdminAccess(false, []string{"10.0.0.0/8"})
	require.NoError(t, err)
	r := adminAccessRouter(access)

	call := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/thing", nil)
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	assert.Equal(t, http.StatusNoContent, call("10.9.8.7:5000").Code)

	refused := call("203.0.113.1:5000")
	assert.Equal(t, http.StatusForbidden, refused.Code)
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(refused.Body.Bytes(), &body))
	assert.Equal(t, CodeAdminNetworkDenied, body.Code)
}

func TestAdminAccess_ListenerOnlyHidesTheRoutesOnThePublicListener(t *testing.T) {
	access, err := ParseAdminAccess(true, []string{"10.0.0.0/8"})
	require.NoError(t, err)
	r := adminAccessRouter(access)

	call := func(onAdmin bool, remote string) int {
		req := httptest.NewRequest(http.MethodGet, "/admin/thing", nil)
		req.RemoteAddr = remote
		if onAdmin {
			req = req.WithContext(OnAdminListener(req.Context()))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// An allowed address does not make the public listener serve them.
	assert.Equal(t, http.StatusNotFound, call(false, "10.9.8.7:5000"))
	assert.Equal(t, http.StatusNoContent, call(true, "10.9.8.7:5000"))
	// Both limits hold on the admin listener.
	assert.Equal(t, http.StatusForbidden, call(true, "203.0.113.1:5000"))
}
