package kyc

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// The KYC providers (#72). Without one, the applications are reviewed by
// hand: the user uploads the documents here, and an administrator decides.
// With one, such as Sumsub, the user gives the documents to the provider
// through its SDK, with a token that the server asks for, and the
// provider decides; its webhook, verified by its signature, moves the
// application, and an approval grants level 2 as an administrator's does.

// Provider is a KYC provider.
type Provider interface {
	// Name is how the applications name it: their provider column.
	Name() string
	// Start opens the verification of the application ref at the
	// provider, and returns the provider's id of it.
	Start(ctx context.Context, ref string) (string, error)
	// Session returns a token for the provider's SDK, for the
	// application ref.
	Session(ctx context.Context, ref string) (Session, error)
	// Verify checks the signature of a webhook and returns its verdict;
	// ErrWebhookSignature for one that the provider did not sign.
	Verify(header http.Header, body []byte) (Verdict, error)
}

// Session is what the user's app needs to run the provider's SDK.
type Session struct {
	Provider  string    `json:"provider" example:"sumsub"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Outcome is what a webhook says of an application.
type Outcome int

// The outcomes of a webhook.
const (
	// OutcomeNone is an event that changes nothing here.
	OutcomeNone Outcome = iota
	// OutcomePending is an application that the user submitted at the
	// provider, which reviews it.
	OutcomePending
	// OutcomeApproved is a positive verdict.
	OutcomeApproved
	// OutcomeRejected is a final negative verdict.
	OutcomeRejected
	// OutcomeRetry is a negative verdict that the user may answer with
	// new documents.
	OutcomeRetry
)

// Verdict is a webhook of the provider about an application.
type Verdict struct {
	// Ref is the application, as the server named it to the provider,
	// and ProviderRef the provider's id of it.
	Ref, ProviderRef string
	Outcome          Outcome
	// Labels are the provider's reasons of a negative verdict, for the
	// administrators.
	Labels []string
}

// ErrWebhookSignature is a webhook without a valid signature.
var ErrWebhookSignature = errors.New("kyc: the webhook is not signed by the provider")

// ErrProviderFlow is a manual step, such as an upload, on an application
// that a provider verifies, or the reverse.
var ErrProviderFlow = errors.New("kyc: the application is verified by the provider")
