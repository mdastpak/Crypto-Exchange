// Package redirect holds the redirect policies of the outbound HTTP clients.
//
// Go's default policy follows up to ten redirects to any host. It drops
// Authorization and Cookie when the host changes, but a credential in any
// other header (an API key such as X-API-KEY), in the URL, or in a client
// certificate goes to the new host too, and Authorization goes with a
// redirect from https to plain http on the same host. A client that carries
// a credential therefore follows no redirect: a provider that moves answers
// with an error the operator sees, never with the key at another address.
package redirect

import (
	"errors"
	"net/http"
)

// ErrRefused is the error of a refused redirect, wrapped in the
// *url.Error that the client returns.
var ErrRefused = errors.New("redirect refused")

// maxHops is how many redirects HTTPSOnly follows, as Go's default policy.
const maxHops = 10

// Refuse is the CheckRedirect of a client that carries a credential: it
// follows no redirect.
func Refuse(*http.Request, []*http.Request) error {
	return ErrRefused
}

// HTTPSOnly is the CheckRedirect of a client that carries no credential
// but must not be downgraded, such as the download of a public list that
// its publisher serves from another host: it follows redirects to any host,
// but only over https.
func HTTPSOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxHops {
		return ErrRefused
	}
	if req.URL.Scheme != "https" {
		return ErrRefused
	}
	return nil
}
