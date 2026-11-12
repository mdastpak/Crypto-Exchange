package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/contactclaim"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is a Store in memory: one profile, and its contacts.
type fakeStore struct {
	profile  Profile
	err      error
	contacts []storedContact
	origins  []audit.Event
}

func (f *fakeStore) Contacts(context.Context, string) ([]storedContact, time.Time, error) {
	return f.contacts, time.Now(), f.err
}
func (f *fakeStore) NationalID(context.Context, string) (string, bool, error) { return "", false, nil }
func (f *fakeStore) ContactOwner(context.Context, string, string) (contactclaim.Owner, bool, error) {
	return contactclaim.Owner{}, false, nil
}
func (f *fakeStore) ClaimCheck(context.Context, string, string) (contactclaim.Verdict, error) {
	return contactclaim.NoNationalID, nil
}
func (f *fakeStore) ApplyChange(context.Context, audit.Event, pendingChange, *ownership, string) ([]storedContact, error) {
	return f.contacts, nil
}
func (f *fakeStore) AccountName(context.Context, string) string { return "account" }
func (f *fakeStore) Profile(context.Context, uuid.UUID) (Profile, error) {
	return f.profile, f.err
}
func (f *fakeStore) UpdateProfile(_ context.Context, _ uuid.UUID, edit func(*Profile), origin audit.Event) (Profile, error) {
	if f.err != nil {
		return Profile{}, f.err
	}
	edit(&f.profile)
	f.origins = append(f.origins, origin)
	return f.profile, nil
}

// serveUser runs one request of the signed-in user userID against a router
// of the profile and contact routes of h.
func serveUser(t *testing.T, h *UserHandler, userID, method, path, body string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { auth.SetClaims(c, &auth.Claims{UUID: userID, Role: auth.RoleUser}) })
	r.GET("/profile", h.GetProfile)
	r.PATCH("/profile", h.UpdateProfile)
	r.GET("/contacts", h.ListContacts)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return w.Code, resp
}

// TestProfileRoutes_RunWithoutInfrastructure runs the profile and contact
// routes on a fake store (#48): the handler validates, applies the update
// through the store, masks the contacts, and maps the errors of the store,
// without a database.
func TestProfileRoutes_RunWithoutInfrastructure(t *testing.T) {
	userID := uuid.NewString()
	first := "Sara"
	store := &fakeStore{
		profile:  Profile{UUID: userID, FirstName: &first},
		contacts: []storedContact{{typ: "email", value: "sara@example.com", verifiedAt: time.Now()}},
	}
	h := &UserHandler{Store: store}

	status, resp := serveUser(t, h, userID, http.MethodPatch, "/profile", `{"last_name": "Karimi"}`)
	require.Equal(t, http.StatusOK, status, resp)
	info := resp["info"].(map[string]any)
	assert.Equal(t, "Sara", info["first_name"], "the other fields stay")
	assert.Equal(t, "Karimi", info["last_name"])
	require.Len(t, store.origins, 1, "the update went through the store")

	status, resp = serveUser(t, h, userID, http.MethodPatch, "/profile", `{"theme": "purple"}`)
	assert.Equal(t, http.StatusBadRequest, status, "the handler validates before the store")
	assert.Equal(t, "validation_failed", resp["code"])
	assert.Len(t, store.origins, 1)

	status, resp = serveUser(t, h, userID, http.MethodGet, "/contacts", "")
	require.Equal(t, http.StatusOK, status, resp)
	items := resp["info"].(map[string]any)["items"].([]any)
	require.Len(t, items, 1)
	assert.NotContains(t, items[0].(map[string]any)["value"], "sara@", "a contact is shown masked")

	store.err = errAccountUnavailable
	status, resp = serveUser(t, h, userID, http.MethodGet, "/profile", "")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "account_unavailable", resp["code"])
}
