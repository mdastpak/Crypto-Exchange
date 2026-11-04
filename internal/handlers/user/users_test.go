package user

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// The database-backed profile tests, including authorization, run end to end
// in cmd/server with real sign-ins.

func TestMaskContact(t *testing.T) {
	cases := []struct{ contactType, value, want string }{
		{"email", "alice@example.com", "ali*****@example.com"},
		{"email", "al@example.com", "al*****@example.com"},
		{"email", "not-an-address", "*****"},
		{"mobile", "09121234567", "091*****567"},
		{"mobile", "0912", "*****"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, maskContact(tc.contactType, tc.value), tc.value)
	}
}

func decode(t *testing.T, body string) (profileUpdate, fieldErrors, error) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPatch, "/user/profile", strings.NewReader(body))
	return decodeProfileUpdate(c)
}

func TestDecodeProfileUpdate_RejectsOtherFields(t *testing.T) {
	for _, field := range []string{"uuid", "status", "contact", "kyc_tier", "role"} {
		_, errs, err := decode(t, `{"`+field+`": "x"}`)
		require.NoError(t, err)
		assert.Equal(t, fieldErrors{field: "is not accepted"}, errs, field)
	}
}

func TestDecodeProfileUpdate_RejectsMalformedBodies(t *testing.T) {
	for _, body := range []string{``, `[]`, `{"first_name": 1}`, `{"first_name": "a"} {"last_name": "b"}`, `{`} {
		_, _, err := decode(t, body)
		assert.ErrorIs(t, err, errMalformedProfileUpdate, body)
	}
}

func TestProfileUpdate_Validate(t *testing.T) {
	text := func(s string) *string { return &s }
	long := strings.Repeat("ب", maxNameLength+1)

	assert.Nil(t, profileUpdate{FirstName: text("  Ali  ")}.validate())
	assert.Nil(t, profileUpdate{DisplayName: text("")}.validate(), "an empty name clears the field")
	assert.Nil(t, profileUpdate{LastName: text("محمدی‌پور")}.validate(), "the zero-width non-joiner is part of Persian names")
	assert.Nil(t, profileUpdate{LastName: text(strings.Repeat("ب", maxNameLength))}.validate(), "the limit counts characters, not bytes")

	assert.Equal(t, fieldErrors{"body": "send at least one of first_name, last_name, and display_name"}, profileUpdate{}.validate())
	assert.Equal(t, fieldErrors{"first_name": "must be at most 100 characters"}, profileUpdate{FirstName: &long}.validate())
	assert.Equal(t, fieldErrors{"display_name": "must not contain control characters"}, profileUpdate{DisplayName: text("a\nb")}.validate())
	assert.Equal(t, fieldErrors{"display_name": "must not contain control characters"}, profileUpdate{DisplayName: text("admin‮gnp.exe")}.validate())
}

func TestProfileUpdate_Apply(t *testing.T) {
	text := func(s string) *string { return &s }
	p := Profile{FirstName: text("Ali"), LastName: text("Rezaei"), DisplayName: text("ali")}

	profileUpdate{FirstName: text("  Sara "), DisplayName: text("")}.apply(&p)

	assert.Equal(t, "Sara", *p.FirstName, "names are trimmed")
	assert.Equal(t, "Rezaei", *p.LastName, "a field that is not sent keeps its value")
	assert.Nil(t, p.DisplayName, "an empty name clears the field")
}

func TestGetProfile_WithoutClaimsIsUnauthorized(t *testing.T) {
	h := &UserHandler{}
	router := gin.New()
	router.GET("/user/profile", h.GetProfile)
	router.PATCH("/user/profile", h.UpdateProfile)

	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, "/user/profile", strings.NewReader(`{"first_name": "a"}`)))
		assert.Equal(t, http.StatusUnauthorized, w.Code, method)
	}
}
