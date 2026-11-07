package models

// APIResponse defines the standard structure for all API responses
type APIResponse struct {
	Status  int         `json:"status"`
	Message string      `json:"message"`
	Info    interface{} `json:"info,omitempty"` // Info can hold any type of data (e.g., list or object)
	// Code names the error of a response of status 400 or more, for
	// clients to act on; Message is for people and may change.
	Code string `json:"code,omitempty"`
	// RequestID is the X-Request-ID of an error response, to quote when
	// reporting it.
	RequestID string `json:"request_id,omitempty"`
}
