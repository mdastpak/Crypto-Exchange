package source

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

var (
	// tags matches HTML tags and comments; block tags become line breaks.
	// scripts and styles go with their content, which is code, not text.
	scripts    = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	styles     = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	blockTags  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6])\s*/?>`)
	anyTag     = regexp.MustCompile(`(?s)<!--.*?-->|<[^>]*>`)
	spaceRuns  = regexp.MustCompile(`[ \t\f\v\r]+`)
	blankLines = regexp.MustCompile(`\n\s*\n+`)
)

// MaxDescription bounds a description, in characters.
const MaxDescription = 20000

// PlainText turns the HTML or text of a source's description into plain
// text: tags removed, entities decoded, spaces collapsed, paragraphs kept,
// and the length bounded. The catalog never stores or serves the source's
// HTML; whoever shows the text escapes it as any text.
func PlainText(s string) string {
	s = scripts.ReplaceAllString(s, "")
	s = styles.ReplaceAllString(s, "")
	s = blockTags.ReplaceAllString(s, "\n")
	s = anyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// Entities may have hidden tags, such as &lt;script&gt;; a second pass
	// removes those too.
	s = anyTag.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' && r != 0x7f && r != utf8.RuneError {
			return r
		}
		return -1
	}, s)
	s = spaceRuns.ReplaceAllString(s, " ")
	s = blankLines.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	s = strings.TrimSpace(strings.Join(lines, "\n"))
	if utf8.RuneCountInString(s) > MaxDescription {
		s = string([]rune(s)[:MaxDescription])
	}
	return s
}

// HTTPSLink returns link when it is a clean https URL of at most 500
// characters, and "" otherwise, so that no javascript:, data:, or plain
// http link reaches a page.
func HTTPSLink(link string) string {
	link = strings.TrimSpace(link)
	if len(link) > 500 || !strings.HasPrefix(strings.ToLower(link), "https://") || strings.ContainsAny(link, " \"'<>\\\n\r\t") {
		return ""
	}
	return "https://" + link[len("https://"):]
}

// FirstHTTPSLink returns the first clean https link of links, or "".
func FirstHTTPSLink(links []string) string {
	for _, l := range links {
		if clean := HTTPSLink(l); clean != "" {
			return clean
		}
	}
	return ""
}

// networks maps the names that sources give networks to the catalog's
// names, so that the same contract matches across sources. A name that is
// not listed is kept, in lower case.
var networks = map[string]string{
	// CoinGecko platform ids.
	"ethereum":            "ethereum",
	"tron":                "tron",
	"binance-smart-chain": "bsc",
	"polygon-pos":         "polygon",
	"solana":              "solana",
	"arbitrum-one":        "arbitrum",
	"optimistic-ethereum": "optimism",
	"base":                "base",
	"avalanche":           "avalanche",
	"the-open-network":    "ton",
	// CoinMarketCap platform slugs.
	"bnb":                     "bsc",
	"bnb-smart-chain-bep20":   "bsc",
	"polygon":                 "polygon",
	"polygon-ecosystem-token": "polygon",
	"arbitrum":                "arbitrum",
	"optimism-ethereum":       "optimism",
	"avalanche-c-chain":       "avalanche",
	"toncoin":                 "ton",
}

// Network returns the catalog's name of a network that a source names
// name.
func Network(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if n, ok := networks[key]; ok {
		return n
	}
	return marketdata.Slugify(key)
}
