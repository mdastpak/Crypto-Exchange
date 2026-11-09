package user

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

const (
	// maxNameLength is the length of the name columns, in characters.
	maxNameLength         = 100
	maxProfileRequestBody = 4 << 10
)

// profileUpdate is the body of PATCH /user/profile. A field that is not sent
// keeps its value, and an empty string clears a name or the locale. The
// other profile fields,
// such as the contacts and the KYC state, cannot be changed here, and
// decodeProfileUpdate rejects them.
type profileUpdate struct {
	FirstName   *string `json:"first_name"`
	LastName    *string `json:"last_name"`
	DisplayName *string `json:"display_name"`
	// Locale is a tag of GET /public/locales, or "" to clear the choice
	// (#286).
	Locale *string `json:"locale" example:"fa"`
	// Theme is light, dark, or system.
	Theme *string `json:"theme" enums:"light,dark,system" example:"dark"`
}

// themes are the colour themes a user may choose.
var themes = []string{"light", "dark", "system"}

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

// validate checks the fields that are sent, after trimming spaces. A
// locale must be one of locales.
func (u profileUpdate) validate(locales *i18n.Registry) fieldErrors {
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
	if u.Locale != nil {
		sent = true
		if code := strings.TrimSpace(*u.Locale); code != "" {
			if _, ok := lookupLocale(locales, code); !ok {
				errs["locale"] = "must be one of the supported locales"
			}
		}
	}
	if u.Theme != nil {
		sent = true
		if !slices.Contains(themes, *u.Theme) {
			errs["theme"] = "must be light, dark, or system"
		}
	}
	if !sent {
		errs["body"] = "send at least one of first_name, last_name, display_name, locale, and theme"
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

// lookupLocale returns the supported locale that code names exactly, so
// that the profile keeps a tag that GET /public/locales lists.
func lookupLocale(locales *i18n.Registry, code string) (i18n.Locale, bool) {
	if locales == nil {
		return i18n.Locale{}, false
	}
	for _, l := range locales.Locales() {
		if strings.EqualFold(l.Code, code) {
			return l, true
		}
	}
	return i18n.Locale{}, false
}

// apply sets the fields of the update on the profile. An empty name or
// locale clears the field. The fields are valid.
func (u profileUpdate) apply(p *Profile, locales *i18n.Registry) {
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
	if u.Locale != nil {
		p.Locale = nil
		if l, ok := lookupLocale(locales, strings.TrimSpace(*u.Locale)); ok {
			p.Locale = &l.Code
		}
	}
	if u.Theme != nil {
		p.Theme = *u.Theme
	}
}

// profileChanges returns the fields that differ between before and after,
// as an audit change, or nil when none does.
func profileChanges(before, after Profile) *audit.Changes {
	c := &audit.Changes{Before: map[string]interface{}{}, After: map[string]interface{}{}}
	for _, field := range []struct {
		name          string
		before, after *string
	}{
		{"first_name", before.FirstName, after.FirstName},
		{"last_name", before.LastName, after.LastName},
		{"display_name", before.DisplayName, after.DisplayName},
		{"locale", before.Locale, after.Locale},
		{"theme", &before.Theme, &after.Theme},
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
