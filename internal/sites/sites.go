// Package sites holds the sites of the product (#285): the domain of each,
// the countries it serves, and its locales. It decides which site and
// locale a visitor belongs to, from their country (internal/geoip), their
// explicit choices, and their browser (internal/i18n), and builds the URL
// to send them to.
//
// The table is configuration, the country is found per request, and the
// visitor's choice is a cookie, so every instance decides the same with no
// shared state.
package sites

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/geoip"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
)

// Spec is the configuration of one site.
type Spec struct {
	ID        string
	Domain    string
	Aliases   []string
	Countries []string
	Locales   []string
	Enforce   bool
}

// Site is one site of the product.
type Site struct {
	// ID names the site, such as "global" or "ir".
	ID string `json:"id" example:"ir"`
	// Domain is its canonical host, such as "example.ir".
	Domain string `json:"domain" example:"example.ir"`
	// URL is the origin of the site, such as "https://example.ir".
	URL string `json:"url" example:"https://example.ir"`
	// Countries are the ISO 3166-1 alpha-2 codes of the countries it
	// serves; empty for a site that serves the rest of the world.
	Countries []string `json:"countries"`
	// Locales are the codes of its locales; the first is its default.
	Locales []string `json:"locales" example:"fa,en"`
	// DefaultLocale is the first of Locales.
	DefaultLocale string `json:"default_locale" example:"fa"`
	// Enforce keeps the visitors of its countries on it, whatever they
	// chose, for a legal requirement.
	Enforce bool `json:"enforce"`

	aliases []string
	locales []i18n.Locale
}

// Offers returns the locale of the site that l names, by its code or else
// by its language, and false when the site offers none of them.
func (s *Site) Offers(l i18n.Locale) (i18n.Locale, bool) {
	for _, own := range s.locales {
		if own.Code == l.Code {
			return own, true
		}
	}
	for _, own := range s.locales {
		if own.Base() == l.Base() {
			return own, true
		}
	}
	return i18n.Locale{}, false
}

// Default returns the default locale of the site.
func (s *Site) Default() i18n.Locale {
	return s.locales[0]
}

// Registry is the table of sites. It does not change after New, so it is
// safe for concurrent use. A nil or empty Registry has no sites: every
// visitor stays where they are.
type Registry struct {
	sites     []*Site
	byID      map[string]*Site
	byHost    map[string]*Site
	byCountry map[string]*Site
	def       *Site
	scheme    string
	locales   *i18n.Registry
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// New builds the table. scheme is "https", or "http" for development.
// Every site has a unique ID and domain, locales of the i18n registry, and
// countries that no other site serves; defaultID names the site of the
// visitors of every other country. No specs means no sites.
func New(defaultID, scheme string, specs []Spec, locales *i18n.Registry) (*Registry, error) {
	r := &Registry{
		byID:      map[string]*Site{},
		byHost:    map[string]*Site{},
		byCountry: map[string]*Site{},
		scheme:    scheme,
		locales:   locales,
	}
	if len(specs) == 0 {
		return r, nil
	}
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("scheme %q must be https or http", scheme)
	}
	for _, spec := range specs {
		site, err := r.add(spec)
		if err != nil {
			return nil, err
		}
		r.sites = append(r.sites, site)
	}
	r.def = r.byID[strings.TrimSpace(defaultID)]
	if r.def == nil {
		return nil, fmt.Errorf("the default site %q is not one of the sites (%s)", defaultID, strings.Join(r.IDs(), ", "))
	}
	return r, nil
}

func (r *Registry) add(spec Spec) (*Site, error) {
	id := strings.TrimSpace(spec.ID)
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("site id %q must be 1 to 32 lower-case letters, digits, and hyphens", spec.ID)
	}
	if r.byID[id] != nil {
		return nil, fmt.Errorf("site %q is listed twice", id)
	}
	site := &Site{ID: id, Enforce: spec.Enforce, Countries: []string{}}
	r.byID[id] = site

	hosts := append([]string{spec.Domain}, spec.Aliases...)
	for i, raw := range hosts {
		host, err := NormalizeHost(raw)
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", id, err)
		}
		if other := r.byHost[host]; other != nil {
			return nil, fmt.Errorf("site %q: domain %q is also a domain of site %q", id, host, other.ID)
		}
		r.byHost[host] = site
		if i == 0 {
			site.Domain = host
		} else {
			site.aliases = append(site.aliases, host)
		}
	}
	site.URL = r.scheme + "://" + site.Domain

	for _, raw := range spec.Countries {
		country, ok := geoip.NormalizeCountry(raw)
		if !ok {
			return nil, fmt.Errorf("site %q: %q is not an ISO 3166-1 alpha-2 country code", id, raw)
		}
		if other := r.byCountry[country]; other != nil {
			return nil, fmt.Errorf("site %q: country %s is also a country of site %q", id, country, other.ID)
		}
		r.byCountry[country] = site
		site.Countries = append(site.Countries, country)
	}
	if spec.Enforce && len(site.Countries) == 0 {
		return nil, fmt.Errorf("site %q enforces its countries, but lists none", id)
	}

	if len(spec.Locales) == 0 {
		return nil, fmt.Errorf("site %q lists no locales", id)
	}
	for _, code := range spec.Locales {
		canonical, err := i18n.ParseCode(code)
		if err != nil {
			return nil, fmt.Errorf("site %q: locale %w", id, err)
		}
		var found *i18n.Locale
		for _, l := range r.locales.Locales() {
			if l.Code == canonical {
				found = &l
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("site %q: locale %q is not one of i18n.locales (%s)", id, canonical, strings.Join(r.locales.Codes(), ", "))
		}
		if slices.Contains(site.Locales, found.Code) {
			return nil, fmt.Errorf("site %q: locale %q is listed twice", id, found.Code)
		}
		site.locales = append(site.locales, *found)
		site.Locales = append(site.Locales, found.Code)
	}
	site.DefaultLocale = site.Locales[0]
	return site, nil
}

// NormalizeHost returns a host name, with an optional port, in lower
// case. It refuses a scheme, a path, user information, and anything that
// is not a host name or an IPv4 address.
func NormalizeHost(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if host == "" {
		return "", fmt.Errorf("a domain is empty")
	}
	if strings.ContainsAny(host, "/\\@?# ") {
		return "", fmt.Errorf("domain %q must be a host name, without a scheme or a path", raw)
	}
	name := host
	if strings.Contains(host, ":") {
		h, port, err := net.SplitHostPort(host)
		if err != nil || !portPattern.MatchString(port) {
			return "", fmt.Errorf("domain %q must be a host name with an optional port", raw)
		}
		name = h
	}
	if ip := net.ParseIP(name); ip == nil || ip.To4() == nil {
		if !hostPattern.MatchString(name) {
			return "", fmt.Errorf("domain %q is not a host name", raw)
		}
	}
	return host, nil
}

var (
	hostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	portPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// Enabled reports whether there are sites.
func (r *Registry) Enabled() bool {
	return r != nil && len(r.sites) > 0
}

// Sites returns the sites, in their configured order.
func (r *Registry) Sites() []*Site {
	if r == nil {
		return nil
	}
	return append([]*Site(nil), r.sites...)
}

// IDs returns the IDs of the sites.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.sites))
	for _, s := range r.Sites() {
		ids = append(ids, s.ID)
	}
	return ids
}

// Default returns the site of the visitors whose country no site serves.
func (r *Registry) Default() *Site {
	if r == nil {
		return nil
	}
	return r.def
}

// ByID returns the site of an ID.
func (r *Registry) ByID(id string) *Site {
	if r == nil {
		return nil
	}
	return r.byID[strings.ToLower(strings.TrimSpace(id))]
}

// ByHost returns the site of a host, by its domain or an alias, or nil.
func (r *Registry) ByHost(host string) *Site {
	if r == nil {
		return nil
	}
	host, err := NormalizeHost(host)
	if err != nil {
		return nil
	}
	if site := r.byHost[host]; site != nil {
		return site
	}
	// A request to the default port names no port.
	if name, port, err := net.SplitHostPort(host); err == nil && (port == "443" || port == "80") {
		return r.byHost[name]
	}
	return nil
}

// ForCountry returns the site of a country, and false when no site serves
// it, so that the default site does.
func (r *Registry) ForCountry(country string) (*Site, bool) {
	if r == nil {
		return nil, false
	}
	if site := r.byCountry[strings.ToUpper(country)]; site != nil {
		return site, true
	}
	return r.def, false
}

// Origins returns the browser origins of every domain of every site, for
// CORS and the WebSocket.
func (r *Registry) Origins() []string {
	var origins []string
	for _, s := range r.Sites() {
		for _, host := range append([]string{s.Domain}, s.aliases...) {
			origins = append(origins, r.scheme+"://"+host)
		}
	}
	return origins
}
