package public

import (
	"net/http"
	"sync"
	"testing"

	"encrypted-db/internal/otp"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestCode requests a code for contact and returns its session ID and the
// code handed to sender.
func requestCode(t *testing.T, router *gin.Engine, sender *recordingOTPSender, contact string) (string, string) {
	t.Helper()
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, status, resp.Message)
	return resp.Info.(map[string]interface{})["uuid"].(string), lastSentCode(t, sender)
}

// verifyInParallel sends the same verification request n times at once and
// counts the response statuses.
func verifyInParallel(t *testing.T, router *gin.Engine, sessionID, code string, n int) map[int]int {
	t.Helper()
	statuses := make(map[int]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			status, _ := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": code})
			mu.Lock()
			statuses[status]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return statuses
}

func newOTPLimitRouter(t *testing.T) (*PublicHandler, *gin.Engine, *recordingOTPSender) {
	t.Helper()
	handler, _, _ := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)
	return handler, router, sender
}

func TestVerifyOTP_RealSchema_ConcurrentGuessesStayWithinTheRetryLimit(t *testing.T) {
	_, router, sender := newOTPLimitRouter(t)
	sessionID, code := requestCode(t, router, sender, "guessed@example.com")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}

	statuses := verifyInParallel(t, router, sessionID, wrong, 30)

	// setupRealSchemaHandler sets otp.auth.retry_limit to 5.
	assert.Equal(t, 5, statuses[http.StatusUnauthorized], "only retry_limit guesses may be checked: %v", statuses)
	assert.Equal(t, 25, statuses[http.StatusTooManyRequests], "%v", statuses)

	status, _ := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": code})
	assert.Equal(t, http.StatusTooManyRequests, status, "the right code is refused once the attempts are used up")
}

func TestVerifyOTP_RealSchema_ConcurrentUseOfACodeSignsInOnce(t *testing.T) {
	_, router, sender := newOTPLimitRouter(t)
	sessionID, code := requestCode(t, router, sender, "raced@example.com")

	statuses := verifyInParallel(t, router, sessionID, code, 10)

	assert.Equal(t, 1, statuses[http.StatusOK], "a code signs in only once: %v", statuses)
	// The other attempts are refused: as unknown once the winner has ended
	// the session, or as too many when they come before that, but after the
	// five that retry_limit allows. Which of the two depends on timing.
	assert.Equal(t, 9, statuses[http.StatusUnauthorized]+statuses[http.StatusTooManyRequests], "%v", statuses)
	assert.LessOrEqual(t, statuses[http.StatusTooManyRequests], 10-5, "only attempts beyond retry_limit count as too many: %v", statuses)
}

func TestRequestOTP_RealSchema_LimitsCodesPerContact(t *testing.T) {
	handler, router, sender := newOTPLimitRouter(t)
	handler.OTP = otp.NewStore(handler.RedisClient.Client, otp.Settings{ContactRequestsPerHour: 2})

	requestCode(t, router, sender, "busy@example.com")
	requestCode(t, router, sender, "busy@example.com")
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": "Busy@Example.com"})

	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "Too many sign-in codes requested. Please try again later.", resp.Message)
	assert.Len(t, sender.messages, 2, "no code is sent beyond the limit")
}

func TestRequestOTP_RealSchema_NewCodesDoNotResetTheAttempts(t *testing.T) {
	handler, router, sender := newOTPLimitRouter(t)
	handler.OTP = otp.NewStore(handler.RedisClient.Client, otp.Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 3})

	sessionID, code := requestCode(t, router, sender, "locked@example.com")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 3; i++ {
		status, _ := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": wrong})
		require.Equal(t, http.StatusUnauthorized, status)
	}

	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": "locked@example.com"})

	assert.Equal(t, http.StatusTooManyRequests, status, "a new code must not bring new attempts")
	assert.Equal(t, "Too many failed attempts. Please try again later.", resp.Message)
	assert.Len(t, sender.messages, 1)
}

func TestVerifyOTP_RealSchema_UnavailableAccountsAreForbidden(t *testing.T) {
	for _, status := range []string{"disabled", "deleted", "suspend"} {
		t.Run(status, func(t *testing.T) {
			handler, router, sender := newOTPLimitRouter(t)
			contact := status + ".account@example.com"
			_, err := handler.PostgresDB.DB.Exec(
				`INSERT INTO users (info, status) VALUES ($1::jsonb, $2)`,
				`{"contact": {"email": "`+contact+`"}}`, status,
			)
			require.NoError(t, err)
			sessionID, code := requestCode(t, router, sender, contact)

			httpStatus, resp := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": code})

			assert.Equal(t, http.StatusForbidden, httpStatus)
			assert.Equal(t, "This account cannot sign in. Please contact support.", resp.Message)
			var sessions int
			require.NoError(t, handler.PostgresDB.DB.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions))
			assert.Zero(t, sessions, "no session may be created")
		})
	}
}
