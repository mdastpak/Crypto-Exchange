// Package kyc runs the KYC applications (#72): a user who verified their
// identity (level 1, #158) applies for level 2 with a form and documents,
// an administrator reviews the application, and an approval grants the
// level in the same transaction.
//
// Personal data is never stored or logged in plaintext. The form is sealed
// with the field key (#140), and each document is encrypted by the service
// with the field key before it leaves for the object storage, which can
// encrypt it again with its own keys; the row of a document holds the
// SHA-256 of what was stored, under the row's seal (#139), so an object
// swapped in the storage is refused. The administrators read a document
// through a link that the service signs for one of them, for a minute, and
// every reading is audited.
package kyc

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Status is the status of an application: the kyc_status of the schema.
type Status string

// The statuses of an application.
const (
	// StatusPending is an application that takes documents.
	StatusPending Status = "PENDING"
	// StatusUnderReview is one submitted, which an administrator reviews.
	StatusUnderReview Status = "UNDER_REVIEW"
	StatusApproved    Status = "APPROVED"
	StatusRejected    Status = "REJECTED"
)

// DocumentType is the type of a document: the document_type of the schema.
type DocumentType string

// The types of documents.
const (
	DocNationalID     DocumentType = "NATIONAL_ID"
	DocPassport       DocumentType = "PASSPORT"
	DocDriversLicense DocumentType = "DRIVERS_LICENSE"
	DocSelfie         DocumentType = "SELFIE"
	DocProofOfAddress DocumentType = "PROOF_OF_ADDRESS"
	DocUtilityBill    DocumentType = "UTILITY_BILL"
	DocBankStatement  DocumentType = "BANK_STATEMENT"
	DocSourceOfFunds  DocumentType = "SOURCE_OF_FUNDS"
	DocCorporate      DocumentType = "CORPORATE_DOCUMENT"
)

// DocumentTypes are the types a user may upload.
var DocumentTypes = []DocumentType{DocNationalID, DocPassport, DocDriversLicense, DocSelfie, DocProofOfAddress,
	DocUtilityBill, DocBankStatement, DocSourceOfFunds, DocCorporate}

// Required are the documents that an application needs before it is
// submitted: one of each group.
var Required = [][]DocumentType{
	{DocNationalID, DocPassport, DocDriversLicense},
	{DocSelfie},
	{DocProofOfAddress, DocUtilityBill, DocBankStatement},
}

// The limits of the documents.
const (
	// MaxDocumentSize is the largest file, in bytes.
	MaxDocumentSize = 10 << 20
	// MaxDocuments is the most documents of an application.
	MaxDocuments = 10
)

// The kinds of files accepted, as their content shows them.
var mimeTypes = []string{"image/jpeg", "image/png", "application/pdf"}

// Form is what a user tells about themselves in an application, besides
// the identity that level 1 verified.
type Form struct {
	Address       string `json:"address" example:"No. 12, Vali Asr St."`
	City          string `json:"city" example:"Tehran"`
	PostalCode    string `json:"postal_code" example:"1234567890"`
	Occupation    string `json:"occupation" example:"Software engineer"`
	SourceOfFunds string `json:"source_of_funds" example:"Salary"`
}

// Invalid is a request with problems, by field.
type Invalid map[string]string

func (i Invalid) Error() string {
	parts := make([]string, 0, len(i))
	for f, p := range i {
		parts = append(parts, f+" "+p)
	}
	return "kyc: invalid: " + strings.Join(parts, "; ")
}

// Validate checks every field.
func (f Form) Validate() error {
	problems := Invalid{}
	for name, c := range map[string]struct {
		value    string
		min, max int
	}{
		"address": {f.Address, 5, 300}, "city": {f.City, 2, 100}, "postal_code": {f.PostalCode, 3, 20},
		"occupation": {f.Occupation, 2, 100}, "source_of_funds": {f.SourceOfFunds, 2, 300},
	} {
		n := utf8.RuneCountInString(strings.TrimSpace(c.value))
		if n < c.min || n > c.max {
			problems[name] = fmt.Sprintf("must be %d to %d characters", c.min, c.max)
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// Application is an application, as the user sees it.
type Application struct {
	ID     string `json:"id" format:"uuid"`
	UserHK string `json:"user_hk,omitempty" format:"uuid"`
	Level  int    `json:"level" example:"2"`
	Status Status `json:"status" enums:"PENDING,UNDER_REVIEW,APPROVED,REJECTED"`
	// Provider verifies the application: manual, with the documents
	// uploaded here, or a provider, such as sumsub, whose SDK takes them
	// with a token of GET /user/kyc/session.
	Provider    string     `json:"provider" enums:"manual,sumsub"`
	Reason      string     `json:"reason,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Documents   []Document `json:"documents"`
}

// Document is a document of an application, without its content.
type Document struct {
	ID        string       `json:"id" format:"uuid"`
	Type      DocumentType `json:"type" enums:"NATIONAL_ID,PASSPORT,DRIVERS_LICENSE,SELFIE,PROOF_OF_ADDRESS,UTILITY_BILL,BANK_STATEMENT,SOURCE_OF_FUNDS,CORPORATE_DOCUMENT"`
	MimeType  string       `json:"mime_type" example:"image/jpeg"`
	Size      int64        `json:"size"`
	CreatedAt time.Time    `json:"created_at"`
}

// Review is an application as an administrator reviews it: with its form,
// opened, and who reviewed it.
type Review struct {
	Application
	Form        Form   `json:"form"`
	ReviewedBy  string `json:"reviewed_by,omitempty" format:"uuid"`
	ReviewNotes string `json:"review_notes,omitempty"`
}

// Link is a link to the content of a document, signed for one
// administrator until a time.
type Link struct {
	URL       string    `json:"url" example:"/api/v1/admin/kyc/documents/4b8f.../content?admin=...&expires=...&sig=..."`
	ExpiresAt time.Time `json:"expires_at"`
}

// Errors of the applications.
var (
	ErrNotFound = errors.New("kyc: no such application or document")
	// ErrLevelRequired is a user below level 1: the identity is verified
	// first (#158).
	ErrLevelRequired = errors.New("kyc: the identity must be verified first")
	// ErrAlreadyVerified is a user at level 2 already.
	ErrAlreadyVerified = errors.New("kyc: the account is verified at level 2 already")
	// ErrOpenApplication is a second application while one is open.
	ErrOpenApplication = errors.New("kyc: an application is open already")
	// ErrNotPending is a change of an application that was submitted.
	ErrNotPending = errors.New("kyc: the application was submitted")
	// ErrNotUnderReview is a review of an application that does not wait
	// for one.
	ErrNotUnderReview = errors.New("kyc: the application does not wait for a review")
	// ErrTooManyDocuments is a document beyond MaxDocuments.
	ErrTooManyDocuments = errors.New("kyc: the application has the most documents")
	// ErrUnavailable is a service without data keys or storage.
	ErrUnavailable = errors.New("kyc: the applications are not available")
	// ErrLinkInvalid is a link to a document that is not signed, or
	// expired.
	ErrLinkInvalid = errors.New("kyc: the link is not valid")
	// ErrContentChanged is a document whose stored content is not what was
	// stored.
	ErrContentChanged = errors.New("kyc: the stored document was changed")
)

// MissingDocuments is a submission without a document of each required
// group; Missing names the groups, each by its types.
type MissingDocuments struct{ Missing [][]DocumentType }

func (m *MissingDocuments) Error() string { return "kyc: documents are missing" }
