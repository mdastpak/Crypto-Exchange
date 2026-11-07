package user

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/gin-gonic/gin"
)

const (
	// maxNameLength is the length of the name columns, in characters.
	maxNameLength         = 100
	maxProfileRequestBody = 4 << 10
)

// profileUpdate is the body of PATCH /user/profile. A field that is not sent
// keeps its value, and an empty string clears it. The other profile fields,
// such as the contacts and the KYC state, cannot be changed here, and
// decodeProfileUpdate rejects them.
type profileUpdate struct {
	FirstName   *string `json:"first_name"`
	LastName    *string `json:"last_name"`
	DisplayName *string `json:"display_name"`
}

// fieldErrors maps a field to what is wrong with it.
type fieldErrors map[string]string

// errMalformedProfileUpdate reports a body that is not a JSON object of the
// expected shape.
var errMalformedProfileUpdate = errors.New("malformed profile update")

// decodeProfileUpdate decodes the body strictly. An unknown field is a field
// error. Other decoding failures return errMalformedProfileUpdate.
func decodeProfileUpdate(c *gin.Context) (profileUpdate, fieldErrors, error) {
	var update profileUpdate
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, maxProfileRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return update, fieldErrors{strings.Trim(name, `"`): "is not accepted"}, nil
		}
		return update, nil, errMalformedProfileUpdate
	}
	if decoder.More() {
		return update, nil, errMalformedProfileUpdate
	}
	return update, nil, nil
}

func (u profileUpdate) fields() map[string]*string {
	return map[string]*string{
		"first_name":   u.FirstName,
		"last_name":    u.LastName,
		"display_name": u.DisplayName,
	}
}

// validate checks the fields that are sent, after trimming spaces.
func (u profileUpdate) validate() fieldErrors {
	errs := fieldErrors{}
	sent := false
	for name, value := range u.fields() {
		if value == nil {
			continue
		}
		sent = true
		if msg := validateName(strings.TrimSpace(*value)); msg != "" {
			errs[name] = msg
		}
	}
	if !sent {
		errs["body"] = "send at least one of first_name, last_name, and display_name"
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func validateName(name string) string {
	if !utf8.ValidString(name) {
		return "must be valid UTF-8"
	}
	if utf8.RuneCountInString(name) > maxNameLength {
		return "must be at most 100 characters"
	}
	visible := false
	for _, r := range name {
		if unicode.IsControl(r) || isBidiOverride(r) {
			return "must not contain control characters"
		}
		if unicode.IsGraphic(r) && !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) {
			visible = true
		}
	}
	if name != "" && !visible {
		return "must contain a visible character"
	}
	return ""
}

// isBidiOverride reports the characters that reorder the text around them,
// which can make a name display as something else. Marks such as the
// zero-width non-joiner, which Persian names need, are allowed.
func isBidiOverride(r rune) bool {
	return (r >= '\u202A' && r <= '\u202E') || (r >= '\u2066' && r <= '\u2069')
}

// apply sets the fields of the update on the profile. An empty name clears
// the field.
func (u profileUpdate) apply(p *Profile) {
	set := func(dst **string, src *string) {
		if src == nil {
			return
		}
		value := strings.TrimSpace(*src)
		if value == "" {
			*dst = nil
			return
		}
		*dst = &value
	}
	set(&p.FirstName, u.FirstName)
	set(&p.LastName, u.LastName)
	set(&p.DisplayName, u.DisplayName)
}

// nameChanges returns the names that differ between before and after, as
// an audit change, or nil when none does.
func nameChanges(before, after Profile) *audit.Changes {
	c := &audit.Changes{Before: map[string]interface{}{}, After: map[string]interface{}{}}
	for _, field := range []struct {
		name          string
		before, after *string
	}{
		{"first_name", before.FirstName, after.FirstName},
		{"last_name", before.LastName, after.LastName},
		{"display_name", before.DisplayName, after.DisplayName},
	} {
		if equalName(field.before, field.after) {
			continue
		}
		c.Before[field.name], c.After[field.name] = nameValue(field.before), nameValue(field.after)
	}
	if len(c.After) == 0 {
		return nil
	}
	return c
}

func equalName(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// nameValue is a name for the audit log, nil when it is not set.
func nameValue(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
