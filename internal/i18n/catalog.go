package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// The texts of the server's messages, one file per language or locale,
// named by its BCP 47 tag: en.json, the source of every key, and the
// translations, such as fa.json. Each maps a message key to its template.
//
//go:embed catalogs/*.json
var catalogFiles embed.FS

// catalog maps the keys of one language to their templates.
type catalog map[Key]*template

// catalogs maps the canonical tag of each catalog to it. They are read
// once, at start; a catalog that does not parse is a build error that the
// tests catch.
var catalogs = mustLoadCatalogs()

func mustLoadCatalogs() map[string]catalog {
	all, err := loadCatalogs()
	if err != nil {
		panic(err)
	}
	return all
}

func loadCatalogs() (map[string]catalog, error) {
	entries, err := catalogFiles.ReadDir("catalogs")
	if err != nil {
		return nil, err
	}
	all := map[string]catalog{}
	for _, entry := range entries {
		name := entry.Name()
		code, err := ParseCode(strings.TrimSuffix(name, path.Ext(name)))
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", name, err)
		}
		data, err := catalogFiles.ReadFile("catalogs/" + name)
		if err != nil {
			return nil, err
		}
		var texts map[Key]string
		if err := json.Unmarshal(data, &texts); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", name, err)
		}
		c := catalog{}
		for key, text := range texts {
			if !keyPattern.MatchString(string(key)) {
				return nil, fmt.Errorf("catalog %s: %q is not a message key: use upper-case letters, digits, and underscores", name, key)
			}
			t, err := parseTemplate(text)
			if err != nil {
				return nil, fmt.Errorf("catalog %s: %s: %w", name, key, err)
			}
			c[key] = t
		}
		all[code] = c
	}
	if _, ok := all[English]; !ok {
		return nil, fmt.Errorf("there is no catalog %s.json, the source of every key", English)
	}
	return all, nil
}

// catalogFor returns the catalog of l: the one of its exact tag, or of its
// language, so that "fa-AF" uses fa.json when it has none of its own.
func catalogFor(l Locale) catalog {
	if c, ok := catalogs[l.Code]; ok {
		return c
	}
	return catalogs[l.Base()]
}

// HasCatalog reports whether there are texts in l. A supported locale
// without a catalog answers in English.
func HasCatalog(l Locale) bool {
	return catalogFor(l) != nil
}

// HasKey reports whether the English catalog defines key.
func HasKey(key Key) bool {
	_, ok := catalogs[English][key]
	return ok
}
