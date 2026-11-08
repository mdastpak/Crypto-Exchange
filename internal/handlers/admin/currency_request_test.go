package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decode runs decodeCurrencyRequest on body, as a handler would.
func decode(t *testing.T, body string) (currencyRequest, fieldErrors, error) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/currencies", strings.NewReader(body))
	return decodeCurrencyRequest(c)
}

func TestDecodeCurrencyRequest_NamesUnknownFieldsByPath(t *testing.T) {
	cases := map[string]struct {
		body  string
		field string
	}{
		"top level":                {`{"hk": "x"}`, "hk"},
		"inside info":              {`{"info": {"foo": 1}}`, "info.foo"},
		"inside info, with others": {`{"status": "pending", "info": {"symbol": "BTC", "decimals": 8}}`, "info.decimals"},
		// The top level is checked first, so a body with both reports the
		// top-level field, as the decoder stops at the first one.
		"both levels": {`{"info": {"foo": 1}, "id": 7}`, "id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, errs, err := decode(t, tc.body)
			require.NoError(t, err)
			assert.Equal(t, fieldErrors{tc.field: "is not accepted"}, errs)
		})
	}
}

func TestDecodeCurrencyRequest_DecodesKnownFields(t *testing.T) {
	req, errs, err := decode(t, `{"status": "approved", "version": 3, "info": {"symbol": "btc", "is_token": false}}`)
	require.NoError(t, err)
	require.Nil(t, errs)
	require.NotNil(t, req.Status)
	assert.Equal(t, "approved", *req.Status)
	require.NotNil(t, req.Version)
	assert.Equal(t, int64(3), *req.Version)
	require.NotNil(t, req.Info.Symbol)
	assert.Equal(t, "btc", *req.Info.Symbol)
	require.NotNil(t, req.Info.IsToken)
	assert.False(t, *req.Info.IsToken)
	assert.Nil(t, req.Info.EnName, "an omitted field stays nil")
}

func TestDecodeCurrencyRequest_InfoMayBeOmittedOrNull(t *testing.T) {
	for _, body := range []string{`{"status": "pending"}`, `{"status": "pending", "info": null}`} {
		req, errs, err := decode(t, body)
		require.NoError(t, err, body)
		assert.Nil(t, errs, body)
		assert.Equal(t, currencyInfoFields{}, req.Info, body)
	}
}

func TestDecodeCurrencyRequest_RejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{
		`{"status": "pending"} {"status": "approved"}`,
		`["status"]`,
		`{"info": "BTC"}`,
		`{"info": {"symbol": 5}}`,
		`{"info": {"symbol": "BTC"`,
	} {
		_, errs, err := decode(t, body)
		assert.ErrorIs(t, err, errMalformedCurrencyRequest, body)
		assert.Nil(t, errs, body)
	}
}
