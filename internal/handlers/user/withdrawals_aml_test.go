package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"
)

// A refusal of the AML rules tells the user no more than that; rules that
// cannot be evaluated, even for a rule changed in SQL, close withdrawals
// for now, as any other dependency that is down (#73).
func TestSendWithdrawalError_AML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for err, want := range map[error]struct {
		status int
		code   string
	}{
		withdrawals.ErrRefused: {http.StatusForbidden, codeWithdrawalRefused},
		fmt.Errorf("%w: %w", aml.ErrUnavailable, rowseal.ErrUnverified): {http.StatusServiceUnavailable, codeWithdrawalsUnavailable},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		sendWithdrawalError(c, "user", err)
		assert.Equal(t, want.status, w.Code, err.Error())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, want.code, resp["code"])
		assert.NotContains(t, w.Body.String(), "aml", "nothing names the rules")
	}
}
