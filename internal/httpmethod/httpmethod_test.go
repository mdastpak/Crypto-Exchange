package httpmethod

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalize_KeepsTheStandardMethodsOnly(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		assert.Equal(t, m, Normalize(m))
	}
	for _, m := range []string{"", "get", "Post", "CONNECT", "TRACE", "PROPFIND", "X-1", "GETX"} {
		assert.Equal(t, Other, Normalize(m), m)
	}
}
