// Package sanctions screens names and addresses on chain against the
// sanctions lists (#73): the OFAC list of Specially Designated Nationals,
// downloaded on a schedule, stored sealed, and matched in memory by every
// instance. It is the screening provider of the AML rules of type
// SANCTIONS; the other providers, such as Chainalysis and TRM, implement
// aml.Screener the same way.
package sanctions

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Address is an address on chain of an entry, and the currency that the
// list names it for, such as XBT or ETH.
type Address struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

// Entry is a sanctioned person, entity, vessel, or aircraft.
type Entry struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Programs  []string  `json:"programs,omitempty"`
	Aliases   []string  `json:"aliases,omitempty"`
	Addresses []Address `json:"addresses,omitempty"`
}

// The types of entries; the list names the entities by none.
const (
	TypeIndividual = "individual"
	TypeEntity     = "entity"
	TypeVessel     = "vessel"
	TypeAircraft   = "aircraft"
)

// List is a version of a list, indexed for matching.
type List struct {
	Provider  string
	Version   int64
	FetchedAt time.Time
	Entries   []Entry

	names     []indexedName
	addresses map[string][]int
}

// indexedName is a name or an alias of an entry, normalized.
type indexedName struct {
	entry int
	key   string
}

// NewList indexes entries.
func NewList(provider string, version int64, fetchedAt time.Time, entries []Entry) *List {
	l := &List{Provider: provider, Version: version, FetchedAt: fetchedAt, Entries: entries, addresses: map[string][]int{}}
	for i, e := range entries {
		if e.Type == TypeVessel || e.Type == TypeAircraft {
			continue
		}
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			if key := NormalizeName(name); strings.Contains(key, " ") {
				l.names = append(l.names, indexedName{entry: i, key: key})
			}
		}
		for _, a := range e.Addresses {
			key := NormalizeAddress(a.Value)
			l.addresses[key] = append(l.addresses[key], i)
		}
	}
	return l
}

// NormalizeName is the form in which names are compared: lower case,
// without accents, punctuation, or the order of their words. "HOSSEINI,
// Ali" and "Ali Hosseini" are one name.
func NormalizeName(name string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(name) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	sort.Strings(words)
	return strings.Join(words, " ")
}

// NormalizeAddress is the form in which addresses are compared: hex
// addresses in lower case, as their checksum is in the case; the others,
// such as base58, as they are.
func NormalizeAddress(address string) string {
	a := strings.TrimSpace(address)
	if strings.HasPrefix(a, "0x") || strings.HasPrefix(a, "0X") {
		return strings.ToLower(a)
	}
	return a
}

// Match is an entry that a name or an address matched.
type Match struct {
	Entry Entry
	// Score is the similarity of the names, 1 for the same name.
	Score float64
}

// MatchName returns the entries one of whose names is at least threshold
// similar to name, by Jaro-Winkler over the normalized names, the best
// first. A name of one word matches nothing: it is not enough to tell a
// person.
func (l *List) MatchName(name string, threshold float64) []Match {
	q := NormalizeName(name)
	if !strings.Contains(q, " ") {
		return nil
	}
	best := map[int]float64{}
	for _, n := range l.names {
		if !similarLength(q, n.key) {
			continue
		}
		score := 1.0
		if n.key != q {
			score = jaroWinkler(q, n.key)
		}
		if score >= threshold && score > best[n.entry] {
			best[n.entry] = score
		}
	}
	out := make([]Match, 0, len(best))
	for i, score := range best {
		out = append(out, Match{Entry: l.Entries[i], Score: score})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Entry.ID < out[j].Entry.ID
	})
	return out
}

// similarLength skips the names whose lengths are too far apart to reach
// any threshold worth using.
func similarLength(a, b string) bool {
	la, lb := len(a), len(b)
	return la*10 >= lb*7 && lb*10 >= la*7
}

// MatchAddress returns the entries that list the address.
func (l *List) MatchAddress(address string) []Match {
	var out []Match
	for _, i := range l.addresses[NormalizeAddress(address)] {
		out = append(out, Match{Entry: l.Entries[i], Score: 1})
	}
	return out
}

// jaroWinkler is the Jaro-Winkler similarity of a and b, from 0 to 1.
func jaroWinkler(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	window := max(len(ra), len(rb))/2 - 1
	window = max(window, 0)
	ma, mb := make([]bool, len(ra)), make([]bool, len(rb))
	matches := 0
	for i := range ra {
		for j := max(0, i-window); j < min(len(rb), i+window+1); j++ {
			if !mb[j] && ra[i] == rb[j] {
				ma[i], mb[j] = true, true
				matches++
				break
			}
		}
	}
	if matches == 0 {
		return 0
	}
	transpositions, k := 0, 0
	for i := range ra {
		if !ma[i] {
			continue
		}
		for !mb[k] {
			k++
		}
		if ra[i] != rb[k] {
			transpositions++
		}
		k++
	}
	m := float64(matches)
	jaro := (m/float64(len(ra)) + m/float64(len(rb)) + (m-float64(transpositions)/2)/m) / 3
	prefix := 0
	for prefix < min(4, len(ra), len(rb)) && ra[prefix] == rb[prefix] {
		prefix++
	}
	return jaro + float64(prefix)*0.1*(1-jaro)
}
