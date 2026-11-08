package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// The translations of the server's messages, one file per language or
// locale, named by its BCP 47 tag, such as fa.json. English needs none: it
// is the language that the handlers write.
//
//go:embed catalogs/*.json
var catalogFiles embed.FS

// catalog is the translations into one language.
type catalog struct {
	// Messages translates a message by its exact English text.
	Messages map[string]string `json:"messages"`
	// Codes translates the message of an error by its code, for the
	// messages that vary, such as one that names the channel of a code.
	Codes map[string]string `json:"codes"`
}

// catalogs maps the canonical tag of each catalog to it. It is read once,
// at start; a catalog that does not parse is a build error that the tests
// catch.
var catalogs = mustLoadCatalogs()

func mustLoadCatalogs() map[string]*catalog {
	all, err := loadCatalogs()
	if err != nil {
		panic(err)
	}
	return all
}

func loadCatalogs() (map[string]*catalog, error) {
	entries, err := catalogFiles.ReadDir("catalogs")
	if err != nil {
		return nil, err
	}
	all := map[string]*catalog{}
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
		var c catalog
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", name, err)
		}
		all[code] = &c
	}
	return all, nil
}

// catalogFor returns the catalog of l: the one of its exact tag, or of its
// language, so that "fa-AF" uses fa.json when it has none of its own.
func catalogFor(l Locale) *catalog {
	if c, ok := catalogs[l.Code]; ok {
		return c
	}
	return catalogs[l.Base()]
}

// HasCatalog reports whether there are translations for l. A supported
// locale without a catalog answers in English.
func HasCatalog(l Locale) bool {
	return l.Base() == English || catalogFor(l) != nil
}
