package venues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Classifier reads the venue's own error of an answer that is not 2xx: its
// code, and whether it is a rejection, or a failure of the venue that the
// call may be tried again after.
type Classifier func(status int, body []byte) (kind Kind, code string)

// Caller calls the API of one venue through a source.Client, which keeps
// the route of the node, the rate limits, the circuit breaker, and the
// bounds of the bodies, and turns every error into an *Error.
type Caller struct {
	Venue    string
	Client   *source.Client
	Classify Classifier
}

// Call makes r for op and decodes a 2xx answer into out, keeping numbers
// as json.Number.
func (c Caller) Call(ctx context.Context, op string, r source.Request, out any) error {
	body, err := c.Client.Do(ctx, r)
	if err != nil {
		return c.wrap(op, r, err)
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		kind := KindRetryable
		if !r.Idempotent {
			// It was done, but its answer cannot be read.
			kind = KindUnknown
		}
		return &Error{Venue: c.Venue, Op: op, Kind: kind, Err: fmt.Errorf("decoding the answer: %w", err)}
	}
	return nil
}

// wrap turns an error of the transport into an *Error.
func (c Caller) wrap(op string, r source.Request, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var status *source.StatusError
	switch {
	case errors.Is(err, source.ErrOutcomeUnknown):
		return &Error{Venue: c.Venue, Op: op, Kind: KindUnknown, Err: err}
	case errors.As(err, &status):
		kind, code := KindRejected, ""
		if c.Classify != nil {
			kind, code = c.Classify(status.Code, status.Body)
		}
		if kind == "" {
			kind = DefaultKind(status.Code)
		}
		switch {
		case kind == KindRetryable && !r.Idempotent && status.Code >= 500:
			kind = KindUnknown
		case kind == KindUnknown && r.Idempotent:
			// A read that failed did nothing.
			kind = KindRetryable
		}
		return &Error{Venue: c.Venue, Op: op, Kind: kind, Code: code, Err: err}
	default:
		// Not sent, or a read that failed: nothing was done.
		return &Error{Venue: c.Venue, Op: op, Kind: KindRetryable, Err: err}
	}
}

// DefaultKind is the kind of an answer by its status alone: 429 and 5xx
// are retryable, any other 4xx a rejection.
func DefaultKind(status int) Kind {
	if status == http.StatusTooManyRequests || status >= 500 {
		return KindRetryable
	}
	return KindRejected
}
