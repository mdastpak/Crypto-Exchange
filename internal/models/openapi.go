package models

// The payloads below are those of success responses that the handlers build
// as maps. They describe them in the OpenAPI document, which swag generates
// from the annotations of the handlers (#54), and must match what the
// handlers send.

// AccessTokenInfo is the info of a completed sign-in or token refresh.
type AccessTokenInfo struct {
	AccessToken string `json:"access_token" example:"eyJhbGciOiJSUzI1NiIsImtpZCI6Ii4uLiJ9..."`
}

// SignInCodeInfo is the info of a sign-in code request of a user.
type SignInCodeInfo struct {
	// Type is the kind of contact the code was sent to.
	Type string `json:"type" enums:"email,mobile" example:"email"`
	// Username is the contact, masked.
	Username string `json:"username" example:"ali*****@example.com"`
	// UUID identifies the sign-in session, for the verification.
	UUID string `json:"uuid" format:"uuid"`
}

// SignInSessionInfo is the info of a sign-in code request of an
// administrator.
type SignInSessionInfo struct {
	// UUID identifies the sign-in session, for the verification.
	UUID string `json:"uuid" format:"uuid"`
}

// CurrencyCreatedInfo is the info of a created currency.
type CurrencyCreatedInfo struct {
	HK      string `json:"hk" format:"uuid"`
	Status  string `json:"status" example:"pending"`
	Version int64  `json:"version" example:"1"`
}

// VersionInfo is the info of an update or a delete of a currency: its new
// version.
type VersionInfo struct {
	Version int64 `json:"version" example:"2"`
}

// PingInfo is the answer of GET /system/ping, which has no envelope.
type PingInfo struct {
	Message   string `json:"message" example:"pong"`
	Timestamp string `json:"timestamp" example:"2026-11-07T10:00:00Z"`
}
