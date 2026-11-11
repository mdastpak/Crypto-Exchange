package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	passkeysRoute        = "/api/v1/user/passkeys"
	passkeyRegisterRoute = "/api/v1/user/passkey-registration"
	passkeySignInRoute   = "/api/v1/public/auth/second-step/passkey"
	passkeyStepUpRoute   = "/api/v1/user/step-up/passkey"

	testRPID   = "exchange.test"
	testOrigin = "https://app.exchange.test"
)

func withPasskeys(c *config.Configuration) {
	c.Security.Passkeys.RPID = testRPID
	c.Security.Passkeys.Origins = []string{testOrigin}
}

// testAuthenticator is a device that holds one passkey: an ES256 key that
// signs what a browser would hand it. It verifies its holder.
type testAuthenticator struct {
	key     *ecdsa.PrivateKey
	id      []byte
	counter uint32
	origin  string
	rpID    string
}

func newTestAuthenticator(t *testing.T) *testAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &testAuthenticator{key: key, id: id, origin: testOrigin, rpID: testRPID}
}

var b64 = base64.RawURLEncoding

// challengeOfOptions returns the challenge of the options of a ceremony.
func challengeOfOptions(t *testing.T, info map[string]interface{}) string {
	t.Helper()
	publicKey, _ := info["publicKey"].(map[string]interface{})
	require.NotNil(t, publicKey, "%v", info)
	challenge, _ := publicKey["challenge"].(string)
	require.NotEmpty(t, challenge)
	return challenge
}

func (a *testAuthenticator) clientData(t *testing.T, kind, challenge string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{"type": kind, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	require.NoError(t, err)
	return raw
}

func (a *testAuthenticator) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte(a.rpID))
	a.counter++
	data := append([]byte{}, rp[:]...)
	data = append(data, flags)
	data = binary.BigEndian.AppendUint32(data, a.counter)
	return append(data, attested...)
}

// create answers navigator.credentials.create.
func (a *testAuthenticator) create(t *testing.T, challenge string) map[string]interface{} {
	t.Helper()
	pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
	cose, err := cbor.Marshal(map[int]interface{}{1: 2, 3: -7, -1: 1, -2: pad(a.key.X.Bytes()), -3: pad(a.key.Y.Bytes())})
	require.NoError(t, err)
	attested := make([]byte, 16) // AAGUID
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(a.id)))
	attested = append(attested, a.id...)
	attested = append(attested, cose...)
	// User present, user verified, attested credential data.
	object, err := cbor.Marshal(map[string]interface{}{"fmt": "none", "attStmt": map[string]interface{}{}, "authData": a.authData(0x45, attested)})
	require.NoError(t, err)
	return map[string]interface{}{
		"id": b64.EncodeToString(a.id), "rawId": b64.EncodeToString(a.id), "type": "public-key",
		"response": map[string]interface{}{
			"clientDataJSON":    b64.EncodeToString(a.clientData(t, "webauthn.create", challenge)),
			"attestationObject": b64.EncodeToString(object),
		},
	}
}

// get answers navigator.credentials.get.
func (a *testAuthenticator) get(t *testing.T, challenge string) map[string]interface{} {
	t.Helper()
	clientData := a.clientData(t, "webauthn.get", challenge)
	authData := a.authData(0x05, nil) // user present, user verified
	hash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), hash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	require.NoError(t, err)
	return map[string]interface{}{
		"id": b64.EncodeToString(a.id), "rawId": b64.EncodeToString(a.id), "type": "public-key",
		"response": map[string]interface{}{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(signature),
		},
	}
}

// addPasskey registers the passkey of device for the user of token, who
// has a password and a fresh proof of it, and returns its ID.
func (e *e2eEnv) addPasskey(t *testing.T, token string, device *testAuthenticator, name string) string {
	t.Helper()
	status, resp := e.post(t, passkeyRegisterRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = e.post(t, passkeysRoute, map[string]interface{}{"name": name, "credential": device.create(t, challengeOfOptions(t, resp.Info))}, token)
	require.Equal(t, http.StatusCreated, status, resp.Message)
	require.NotEmpty(t, resp.Info["security_hold_until"])
	return resp.Info["passkey"].(map[string]interface{})["id"].(string)
}

// TestUserPasskey_IsASecondStepInsteadOfThePassword adds a passkey (#307)
// and uses it where the password is asked for.
func TestUserPasskey_IsASecondStepInsteadOfThePassword(t *testing.T) {
	env := newE2EEnv(t, loginLimit, withPasskeys)
	const email = "passkey@example.com"
	token, _ := env.signedIn(t, email)

	// A passkey comes after the password, and needs a fresh proof of it.
	status, resp := env.post(t, passkeyRegisterRoute, nil, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "second_factor_not_set", resp.Code)
	env.setPassword(t, token, testPassword)
	status, resp = env.post(t, passkeyRegisterRoute, nil, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "step_up_required", resp.Code)
	env.stepUpWithPassword(t, token)

	// The answer must be for the registration that began, from the
	// origin of the exchange.
	device := newTestAuthenticator(t)
	status, resp = env.post(t, passkeyRegisterRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	challenge := challengeOfOptions(t, resp.Info)
	phisher := *device
	phisher.origin = "https://app.exchange.test.evil.example"
	status, resp = env.post(t, passkeysRoute, map[string]interface{}{"name": "Phone", "credential": phisher.create(t, challenge)}, token)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "passkey_invalid", resp.Code)
	status, resp = env.post(t, passkeysRoute, map[string]interface{}{"name": "Phone", "credential": device.create(t, challenge)}, token)
	assert.Equal(t, http.StatusBadRequest, status, "a registration is answered once")

	id := env.addPasskey(t, token, device, "Phone")
	status, body := env.sessionCall(t, http.MethodGet, passkeysRoute, token)
	require.Equal(t, http.StatusOK, status, body)
	listed := body["info"].(map[string]interface{})["passkeys"].([]interface{})
	require.Len(t, listed, 1)
	assert.Equal(t, "Phone", listed[0].(map[string]interface{})["name"])
	assert.NotContains(t, listed[0], "credential")
	status, body = env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.EqualValues(t, 1, body["info"].(map[string]interface{})["passkeys"])

	// The same credential is not added twice.
	status, resp = env.post(t, passkeyRegisterRoute, nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, passkeysRoute, map[string]interface{}{"name": "Again", "credential": device.create(t, challengeOfOptions(t, resp.Info))}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "passkey_exists", resp.Code)

	// The sign-in offers the passkey, and its proof finishes it.
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, []interface{}{"password", "passkey"}, resp.Info["methods"])
	assert.Equal(t, []interface{}{"recovery_code", "passkey"}, resp.Info["password_reset"])
	signIn := resp.Info["challenge"].(string)
	status, resp = env.post(t, passkeySignInRoute, map[string]string{"challenge": signIn}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	options := challengeOfOptions(t, resp.Info)

	// A proof for another sign-in, or by another key, proves nothing.
	other := env.challengeOf(t, email)
	status, resp = env.post(t, secondStepRoute, map[string]interface{}{"challenge": other, "passkey": device.get(t, options)}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "passkey_invalid", resp.Code)
	stranger := newTestAuthenticator(t)
	stranger.id = device.id
	status, resp = env.post(t, secondStepRoute, map[string]interface{}{"challenge": signIn, "passkey": stranger.get(t, options)}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "passkey_invalid", resp.Code)

	// A failed answer spends the proof, so a new one begins.
	status, resp = env.post(t, passkeySignInRoute, map[string]string{"challenge": signIn}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, secondStepRoute, map[string]interface{}{"challenge": signIn, "passkey": device.get(t, challengeOfOptions(t, resp.Info))}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	signedIn := resp.Info["access_token"].(string)
	var method string
	require.NoError(t, env.db.QueryRow(`SELECT auth_method FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&method))
	assert.Equal(t, "OTP+PASSKEY", method)

	// A step-up takes a passkey too, bound to its session.
	_, err := env.db.Exec(`UPDATE session_step_ups SET elevated_until = NOW() - INTERVAL '1 second'`)
	require.NoError(t, err)
	status, resp = env.post(t, passkeyStepUpRoute, nil, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)
	proof := device.get(t, challengeOfOptions(t, resp.Info))
	status, resp = env.post(t, stepUpRoute, map[string]interface{}{"passkey": proof}, token)
	assert.Equal(t, http.StatusForbidden, status, "another session cannot use the proof")
	status, resp = env.post(t, passkeyStepUpRoute, nil, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, stepUpRoute, map[string]interface{}{"passkey": device.get(t, challengeOfOptions(t, resp.Info))}, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)

	// Removing it leaves the password.
	status, resp = env.del(t, passkeysRoute+"/"+id, signedIn)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.del(t, passkeysRoute+"/"+id, signedIn)
	assert.Equal(t, http.StatusNotFound, status)
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, []interface{}{"password"}, resp.Info["methods"])
	status, resp = env.post(t, passkeySignInRoute, map[string]string{"challenge": resp.Info["challenge"].(string)}, "")
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "passkey_not_found", resp.Code)

	adds := auditEvents(t, env.db, audit.ActionUserPasskeyAdd)
	require.NotEmpty(t, adds)
	assert.Equal(t, audit.Success, adds[len(adds)-1].Outcome)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserPasskeyRemove), 1)
}

// TestUserPasskey_ResetsAPasswordAndCancelsARecovery checks the passkey as
// the second proof of a password reset, which also stops a recovery.
func TestUserPasskey_ResetsAPasswordAndCancelsARecovery(t *testing.T) {
	env := newE2EEnv(t, loginLimit, withPasskeys)
	const email = "forgot-with-passkey@example.com"
	const next = "a new and better password"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)
	env.stepUpWithPassword(t, token)
	device := newTestAuthenticator(t)
	env.addPasskey(t, token, device, "Laptop")

	status, resp := env.recoverWith(t, env.challengeOf(t, email), "")
	require.Equal(t, http.StatusOK, status, resp.Message)

	challenge := env.challengeOf(t, email)
	status, resp = env.post(t, passkeySignInRoute, map[string]string{"challenge": challenge}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, resetRoute, map[string]interface{}{"challenge": challenge, "passkey": device.get(t, challengeOfOptions(t, resp.Info)), "new_password": next}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, http.StatusUnauthorized, env.profileStatus(t, token), "every older session is ended")
	var open int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM account_recoveries WHERE status IN ('verifying', 'waiting')`).Scan(&open))
	assert.Zero(t, open)
	status, _ = env.secondStep(t, env.challengeOf(t, email), next)
	assert.Equal(t, http.StatusOK, status)
}

// TestUserPasskey_ARowWrittenInSQLProvesNothing checks the seal of the
// passkeys (#139): a credential that was planted in the table is not one.
func TestUserPasskey_ARowWrittenInSQLProvesNothing(t *testing.T) {
	env := newE2EEnv(t, loginLimit, withPasskeys)
	t.Cleanup(rowseal.Use(rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)))
	const email = "planted@example.com"
	token, _ := env.signedIn(t, email)
	env.setPassword(t, token, testPassword)
	env.stepUpWithPassword(t, token)
	device := newTestAuthenticator(t)
	env.addPasskey(t, token, device, "Phone")

	// Someone with access to the database swaps the credential for theirs.
	attacker := newTestAuthenticator(t)
	other, _ := env.signedIn(t, "attacker@example.com")
	env.setPassword(t, other, testPassword)
	env.stepUpWithPassword(t, other)
	env.addPasskey(t, other, attacker, "Mine")
	_, err := e2eOwnerDB(t).Exec(`UPDATE user_passkeys SET user_hk = $1 WHERE name = 'Mine'`, userOf(t, token))
	require.NoError(t, err)

	status, body := env.sessionCall(t, http.MethodGet, passkeysRoute, token)
	require.Equal(t, http.StatusOK, status, body)
	assert.Len(t, body["info"].(map[string]interface{})["passkeys"], 1, "the planted row is left out")
	challenge := env.challengeOf(t, email)
	status, resp := env.post(t, passkeySignInRoute, map[string]string{"challenge": challenge}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, secondStepRoute, map[string]interface{}{"challenge": challenge, "passkey": attacker.get(t, challengeOfOptions(t, resp.Info))}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "passkey_invalid", resp.Code)
}

// TestUserPasskey_OffWithoutARelyingParty checks a deployment without
// security.passkeys.rp_id.
func TestUserPasskey_OffWithoutARelyingParty(t *testing.T) {
	env := newE2EEnv(t)
	token, _ := env.signedIn(t, "no-passkeys@example.com")
	env.setPassword(t, token, testPassword)
	env.stepUpWithPassword(t, token)
	status, resp := env.post(t, passkeyRegisterRoute, nil, token)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "passkey_unavailable", resp.Code)
	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/security", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.Equal(t, false, body["info"].(map[string]interface{})["passkeys_available"])
}
