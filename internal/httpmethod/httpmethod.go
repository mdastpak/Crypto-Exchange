// Package httpmethod bounds the request method where it becomes a label or
// a name, such as a Prometheus label or the name of a span (#403). net/http
// accepts any token as a method, so a client could otherwise make each
// request a new series that is never removed.
package httpmethod

import "net/http"

// Other stands for every method that is not a standard one of the API.
const Other = "other"

// Normalize returns method when it is GET, HEAD, POST, PUT, PATCH, DELETE,
// or OPTIONS, and Other for anything else, including a lower-case form of
// one of them: methods are case-sensitive.
func Normalize(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return Other
}
