package currencies

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/pagination"
)

// The filters, sorts, and pages of the currency lists (#218). The public
// list and the admin list share them, so that a filter means the same on
// both. They run in memory over the whole list: the public list is the
// cached list that every instance shares, and the admin list reads every
// currency once per request. Currencies are few, hundreds or a few thousand,
// so a list is cheap to filter, and the public answer is the same whether
// the cache or the database fallback served it.

// Limits of the query parameters.
const (
	maxTextFilter = 64
	maxSymbol     = 15
	maxContract   = 128
	maxSymbols    = 50
)

// Statuses are the statuses that the admin list can filter by.
var Statuses = []string{models.CurrencyStatusApproved, models.CurrencyStatusPending, models.CurrencyStatusSuspend, models.CurrencyStatusDeleted}

// Sort fields.
const (
	SortSymbol    = "symbol"
	SortEnName    = "en_name"
	SortCreatedAt = "created_at"
	SortUpdatedAt = "updated_at"
)

var sortFields = []string{SortSymbol, SortEnName, SortCreatedAt, SortUpdatedAt}

// Query selects and orders currencies. Its zero value selects every
// currency in the default order.
type Query struct {
	// Text matches symbol, en_name, or fa_name, normalized.
	Text            string
	Symbols         []string
	EnName, FaName  string
	Contract        string
	HasContract     *bool
	IsToken, IfFiat *bool
	Statuses        []string
	// Lifecycles keeps the currencies at one of these lifecycle states.
	Lifecycles   []string
	CreatedSince *time.Time
	CreatedUntil *time.Time
	UpdatedSince *time.Time
	// Sort is a sort field, or empty for the default order; Desc reverses
	// it.
	Sort string
	Desc bool
	// DefaultDesc is the direction of the default order, by creation:
	// oldest first on the public list, newest first on the admin list.
	DefaultDesc bool
	// HKs, when not nil, keeps only these currencies, such as the
	// favorites of a user.
	HKs map[string]bool
}

// Normalize folds s for matching: lower case, Arabic yeh and kaf as their
// Persian forms, Persian and Arabic digits as ASCII, and without the
// zero-width joiners, directional marks, tatweel, and diacritics that the
// same word may be typed with or without.
func Normalize(s string) string {
	s = contact.Digits(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 'ي' || r == 'ى':
			r = 'ی'
		case r == 'ك':
			r = 'ک'
		case r == 'ة':
			r = 'ه'
		case r == '\u200c' || r == '\u200d' || r == '\u200e' || r == '\u200f' || r == '\u0640' || r == '\ufeff':
			continue
		case (r >= '\u064b' && r <= '\u065f') || r == '\u0670':
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// QueryParams are the parameters of the public list. The admin list also
// takes status.
var QueryParams = []string{"q", "symbol", "en_name", "fa_name", "contract", "has_contract", "is_token", "if_fiat",
	"created_since", "created_until", "updated_since", "lifecycle", "sort", "limit", "cursor"}

// ParseQuery reads a query from the URL query of a list. extra names the
// other parameters that the list takes, which the caller reads; status is
// read here when extra holds it. Every other parameter is refused, so that a
// misspelled filter does not return everything. The errors map each invalid
// parameter to what is wrong with it.
func ParseQuery(values url.Values, extra ...string) (Query, map[string]string) {
	var q Query
	errs := map[string]string{}
	for name := range values {
		if !slices.Contains(QueryParams, name) && !slices.Contains(extra, name) {
			errs[name] = "is not a parameter of this list"
		}
	}
	text := func(name string, max int) string {
		raw := strings.TrimSpace(values.Get(name))
		if utf8.RuneCountInString(raw) > max {
			errs[name] = fmt.Sprintf("must be at most %d characters", max)
		}
		if strings.ContainsRune(raw, 0) {
			errs[name] = "must not contain NUL"
		}
		return Normalize(raw)
	}
	q.Text = text("q", maxTextFilter)
	q.EnName = text("en_name", maxTextFilter)
	q.FaName = text("fa_name", maxTextFilter)
	q.Contract = strings.TrimSpace(values.Get("contract"))
	if len(q.Contract) > maxContract {
		errs["contract"] = fmt.Sprintf("must be at most %d characters", maxContract)
	}
	q.Symbols = list(values["symbol"], strings.ToUpper)
	if len(q.Symbols) > maxSymbols {
		errs["symbol"] = fmt.Sprintf("must name at most %d symbols", maxSymbols)
	}
	for _, s := range q.Symbols {
		if len(s) > maxSymbol {
			errs["symbol"] = fmt.Sprintf("must be symbols of at most %d characters", maxSymbol)
		}
	}
	if slices.Contains(extra, "status") {
		q.Statuses = list(values["status"], strings.ToLower)
		for _, s := range q.Statuses {
			if !slices.Contains(Statuses, s) {
				errs["status"] = "must be approved, pending, suspend, or deleted"
			}
		}
	}
	q.Lifecycles = list(values["lifecycle"], strings.ToLower)
	for _, s := range q.Lifecycles {
		if Lifecycle(s).Rank() < 0 {
			errs["lifecycle"] = "must be lifecycle states: active, delist_announced, deposits_disabled, sell_only, trading_halted, withdrawal_only, frozen, or delisted"
		}
	}
	for name, flag := range map[string]**bool{"has_contract": &q.HasContract, "is_token": &q.IsToken, "if_fiat": &q.IfFiat} {
		if raw := values.Get(name); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				errs[name] = "must be true or false"
				continue
			}
			*flag = &v
		}
	}
	for name, at := range map[string]**time.Time{"created_since": &q.CreatedSince, "created_until": &q.CreatedUntil, "updated_since": &q.UpdatedSince} {
		if raw := values.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				errs[name] = "must be an RFC 3339 time"
				continue
			}
			*at = &t
		}
	}
	if raw := values.Get("sort"); raw != "" {
		q.Sort, q.Desc = strings.CutPrefix(raw, "-")
		if !slices.Contains(sortFields, q.Sort) {
			errs["sort"] = "must be symbol, en_name, created_at, or updated_at, with - for descending"
		}
	}
	if len(errs) > 0 {
		return Query{}, errs
	}
	return q, nil
}

// list reads a parameter that may be repeated and comma-separated.
func list(raw []string, fold func(string) string) []string {
	var out []string
	for _, v := range raw {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" && !slices.Contains(out, fold(part)) {
				out = append(out, fold(part))
			}
		}
	}
	return out
}

// Match reports whether q selects c.
func (q Query) Match(c models.Currency) bool {
	if q.HKs != nil && !q.HKs[c.HK] {
		return false
	}
	// A previous symbol finds the currency too, so that a search for MATIC
	// finds POL after the rebrand (#215).
	if q.Text != "" {
		if !strings.Contains(Normalize(c.Info.Symbol), q.Text) && !strings.Contains(Normalize(c.Info.EnName), q.Text) &&
			!strings.Contains(Normalize(c.Info.FaName), q.Text) &&
			!slices.ContainsFunc(c.PreviousSymbols, func(s string) bool { return strings.Contains(Normalize(s), q.Text) }) {
			return false
		}
	}
	if len(q.Symbols) > 0 && !slices.Contains(q.Symbols, strings.ToUpper(c.Info.Symbol)) &&
		!slices.ContainsFunc(c.PreviousSymbols, func(s string) bool { return slices.Contains(q.Symbols, strings.ToUpper(s)) }) {
		return false
	}
	if q.EnName != "" && !strings.Contains(Normalize(c.Info.EnName), q.EnName) {
		return false
	}
	if q.FaName != "" && !strings.Contains(Normalize(c.Info.FaName), q.FaName) {
		return false
	}
	if q.Contract != "" && !strings.EqualFold(strings.TrimSpace(c.Info.Contract), q.Contract) {
		return false
	}
	if q.HasContract != nil && (strings.TrimSpace(c.Info.Contract) != "") != *q.HasContract {
		return false
	}
	if q.IsToken != nil && c.Info.IsToken != *q.IsToken {
		return false
	}
	if q.IfFiat != nil && c.Info.IfFiat != *q.IfFiat {
		return false
	}
	if len(q.Statuses) > 0 && !slices.Contains(q.Statuses, c.Status) {
		return false
	}
	if len(q.Lifecycles) > 0 {
		state := string(LifecycleActive)
		if c.Lifecycle != nil {
			state = c.Lifecycle.State
		}
		if !slices.Contains(q.Lifecycles, state) {
			return false
		}
	}
	if q.CreatedSince != nil || q.CreatedUntil != nil {
		created, ok := parseTime(c.CreatedAt)
		if !ok || (q.CreatedSince != nil && created.Before(*q.CreatedSince)) ||
			(q.CreatedUntil != nil && !created.Before(*q.CreatedUntil)) {
			return false
		}
	}
	if q.UpdatedSince != nil {
		updated, ok := parseTime(c.UpdatedAt)
		if !ok || updated.Before(*q.UpdatedSince) {
			return false
		}
	}
	return true
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// position is the place of a currency in an order, which the cursor holds:
// the order it is of, the rank of a text search, the sort key, and the hk
// that breaks ties.
type position struct {
	Order string `json:"o"`
	Rank  int    `json:"r,omitempty"`
	Key   string `json:"k"`
	HK    string `json:"h"`
}

// order names the order of q, which a cursor must be of.
func (q Query) order() string {
	field, desc := q.Sort, q.Desc
	if field == "" {
		field, desc = SortCreatedAt, q.DefaultDesc
		if q.Text != "" {
			field = "rank," + field
		}
	}
	if desc {
		return "-" + field
	}
	return field
}

// ranked reports whether the order puts the symbols that match the text
// search first: the default order of a search.
func (q Query) ranked() bool { return q.Sort == "" && q.Text != "" }

func (q Query) position(c models.Currency) position {
	p := position{Order: q.order(), HK: c.HK}
	field := q.Sort
	if field == "" {
		field = SortCreatedAt
	}
	switch field {
	case SortSymbol:
		p.Key = strings.ToUpper(c.Info.Symbol)
	case SortEnName:
		p.Key = Normalize(c.Info.EnName)
	case SortCreatedAt:
		// Times have seconds; the id orders the currencies of one second
		// as they were created.
		p.Key = sortableTime(c.CreatedAt) + fmt.Sprintf("/%020d", c.ID)
	case SortUpdatedAt:
		p.Key = sortableTime(c.UpdatedAt)
	}
	if q.ranked() {
		symbol := Normalize(c.Info.Symbol)
		switch {
		case symbol == q.Text:
			p.Rank = 0
		case strings.HasPrefix(symbol, q.Text),
			slices.ContainsFunc(c.PreviousSymbols, func(s string) bool { return Normalize(s) == q.Text }):
			p.Rank = 1
		default:
			p.Rank = 2
		}
	}
	return p
}

// sortableTime formats a time so that its strings sort as the times do.
func sortableTime(s string) string {
	t, ok := parseTime(s)
	if !ok {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// before reports whether a comes before b in the order of q. The rank of a
// search always comes first, best first; the key and the hk follow the
// direction.
func (q Query) before(a, b position) bool {
	if a.Rank != b.Rank {
		return a.Rank < b.Rank
	}
	desc := q.Desc
	if q.Sort == "" {
		desc = q.DefaultDesc
	}
	if a.Key != b.Key {
		return (a.Key < b.Key) != desc
	}
	return (a.HK < b.HK) != desc
}

// Page filters list, orders it, and returns the page of r.
func (q Query) Page(list []models.Currency, r pagination.Request) (pagination.Page[models.Currency], error) {
	type entry struct {
		c models.Currency
		p position
	}
	entries := make([]entry, 0, len(list))
	for _, c := range list {
		if q.Match(c) {
			entries = append(entries, entry{c, q.position(c)})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		switch {
		case q.before(a.p, b.p):
			return -1
		case q.before(b.p, a.p):
			return 1
		}
		return 0
	})

	var after position
	more, err := r.After(&after)
	if err != nil {
		return pagination.Page[models.Currency]{}, err
	}
	start := 0
	if more {
		if after.Order != q.order() || after.HK == "" {
			return pagination.Page[models.Currency]{}, pagination.ErrInvalidCursor
		}
		// The page starts after the position, which stays valid when the
		// currency of the position was removed in the meantime.
		start, _ = slices.BinarySearchFunc(entries, after, func(e entry, p position) int {
			if q.before(e.p, p) || e.p == p {
				return -1
			}
			return 1
		})
	}
	end := min(start+r.Limit+1, len(entries))
	items := make([]models.Currency, 0, end-start)
	for _, e := range entries[start:end] {
		items = append(items, e.c)
	}
	return pagination.NewPage(items, r, func(c models.Currency) interface{} { return q.position(c) })
}
