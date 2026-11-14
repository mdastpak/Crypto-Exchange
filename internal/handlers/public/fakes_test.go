package public

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/otp"
	"github.com/mdastpak/crypto-exchange/internal/signin"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCodes is a CodeStore whose every code is right for contact, or wrong.
type fakeCodes struct {
	contact string
	err     error
}

func (f fakeCodes) Issue(context.Context, string, string) (otp.Issued, error) {
	return otp.Issued{}, nil
}
func (f fakeCodes) Verify(context.Context, string, string) (string, error) { return f.contact, f.err }

// fakeAccounts is an Accounts whose sign-in returns err.
type fakeAccounts struct{ err error }

func (f fakeAccounts) FindOrCreate(context.Context, audit.Event, string, string, signin.SignUp) (uuid.UUID, error) {
	return uuid.Nil, f.err
}
func (f fakeAccounts) Owner(context.Context, string, string) (string, string) {
	return audit.ActorUser, "33333333-3333-3333-3333-333333333333"
}
func (f fakeAccounts) CanSignIn(context.Context, uuid.UUID) (bool, bool, error) {
	return false, false, nil
}
func (f fakeAccounts) ContactID(context.Context, uuid.UUID, string, string) (int64, error) {
	return 0, nil
}
func (f fakeAccounts) Contact(context.Context, string, int64) (string, string, error) {
	return "", "", nil
}
func (f fakeAccounts) NationalID(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (f fakeAccounts) FavoriteCurrencies(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

// fakeAudit records the events of the sign-ins.
type fakeAudit struct{ events []audit.Event }

func (f *fakeAudit) Record(_ context.Context, e audit.Event) error {
	f.events = append(f.events, e)
	return nil
}

// TestVerifyOTP_RunsWithoutInfrastructure runs the code verification of a
// sign-in on fakes (#48): the handler maps the codes and the refusals of
// the accounts to their answers, and records each attempt, without a
// database or Redis.
func TestVerifyOTP_RunsWithoutInfrastructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name         string
		codes        fakeCodes
		accounts     error
		status       int
		code, reason string
	}{
		{"a wrong code", fakeCodes{err: otp.ErrInvalidCode}, nil, http.StatusUnauthorized, "code_invalid", "code_invalid"},
		{"too many codes", fakeCodes{err: otp.ErrTooManyAttempts}, nil, http.StatusTooManyRequests, "", reasonRateLimited},
		{"a frozen account", fakeCodes{contact: "frozen@example.com"}, signin.ErrAccountFrozen, http.StatusForbidden, "account_frozen", "account_frozen"},
		{"a disabled account", fakeCodes{contact: "off@example.com"}, signin.ErrAccountUnavailable, http.StatusForbidden, "account_unavailable", "account_unavailable"},
		{"an unchecked mobile", fakeCodes{contact: "09121234567"}, signin.ErrIdentityUnavailable, http.StatusServiceUnavailable, codeIdentityUnavailable, codeIdentityUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &fakeAudit{}
			h := &PublicHandler{OTP: tc.codes, Accounts: fakeAccounts{err: tc.accounts}, Audit: log}
			r := gin.New()
			r.POST("/auth/:uuid", h.VerifyOTP)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/"+uuid.NewString(), strings.NewReader(`{"otp": "123456"}`)))

			assert.Equal(t, tc.status, w.Code, w.Body.String())
			var resp map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			if tc.code != "" {
				assert.Equal(t, tc.code, resp["code"])
			}
			require.Len(t, log.events, 1, "each attempt is recorded")
			assert.Equal(t, tc.reason, log.events[0].Reason)
			assert.Equal(t, audit.ActionUserSignIn, log.events[0].Action)
		})
	}
}
