package currencies

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func currency(hk, symbol, en, fa, contract string, token, fiat bool, created, updated string) models.Currency {
	return models.Currency{HK: hk, Status: models.CurrencyStatusApproved, CreatedAt: created, UpdatedAt: updated,
		Info: models.CurrencyInfo{Symbol: symbol, EnName: en, FaName: fa, Contract: contract, IsToken: token, IfFiat: fiat}}
}

// sample is a list with Persian names typed as Persian keyboards type them.
var sample = []models.Currency{
	currency("00000000-0000-0000-0000-000000000001", "BTC", "Bitcoin", "بیت\u200cکوین", "", false, false, "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"),
	currency("00000000-0000-0000-0000-000000000002", "ETH", "Ethereum", "اتریوم", "", false, false, "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"),
	currency("00000000-0000-0000-0000-000000000003", "USDT", "Tether", "تتر", "0xdAC17F958D2ee523a2206206994597C13D831ec7", true, false, "2026-01-03T00:00:00Z", "2026-02-01T00:00:00Z"),
	currency("00000000-0000-0000-0000-000000000004", "IRR", "Iranian Rial", "ریال ایران", "", false, true, "2026-01-04T00:00:00Z", "2026-01-04T00:00:00Z"),
	currency("00000000-0000-0000-0000-000000000005", "BTCB", "Bitcoin BEP2", "بیت کوین بی\u200cای\u200cپی ۲", "BTCB-1DE", true, false, "2026-01-04T00:00:00Z", "2026-01-05T00:00:00Z"),
	currency("00000000-0000-0000-0000-000000000006", "WBTC", "Wrapped Bitcoin", "بیت\u200cکوین رپد", "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599", true, false, "2026-01-06T00:00:00Z", "2026-01-06T00:00:00Z"),
}

func parse(t *testing.T, raw string, extra ...string) Query {
	t.Helper()
	values, err := url.ParseQuery(raw)
	require.NoError(t, err)
	q, errs := ParseQuery(values, extra...)
	require.Empty(t, errs, raw)
	return q
}

func request(t *testing.T, raw string) pagination.Request {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?"+raw, nil)
	r, errs := pagination.Parse(c)
	require.Empty(t, errs)
	return r
}

func run(t *testing.T, raw string) []string {
	t.Helper()
	page, err := parse(t, raw).Page(sample, request(t, "limit=200"))
	require.NoError(t, err)
	return symbols(page.Items)
}

func TestNormalize_FoldsWhatTheSameWordIsTypedWith(t *testing.T) {
	// Arabic yeh and kaf, a ZWNJ, a diacritic, tatweel, and Arabic digits.
	assert.Equal(t, Normalize("بیت\u200cکوین ۲"), Normalize("بيتكوين ٢"))
	assert.Equal(t, "بیتکوین 2", Normalize("بیت\u200cکـوینً ۲"))
	assert.Equal(t, "bitcoin", Normalize("  BitCoin "))
}

func TestQuery_EachFilterAloneAndCombined(t *testing.T) {
	for raw, want := range map[string][]string{
		"":                             {"BTC", "ETH", "USDT", "IRR", "BTCB", "WBTC"},
		"symbol=btc":                   {"BTC"},
		"symbol=btc,eth&symbol=irr":    {"BTC", "ETH", "IRR"},
		"en_name=bitcoin":              {"BTC", "BTCB", "WBTC"},
		"fa_name=بيتكوين":              {"BTC", "WBTC"},
		"fa_name=بی\u200cای\u200cپی ٢": {"BTCB"},
		"fa_name=ريال":                 {"IRR"},
		"contract=0XDAC17F958D2EE523A2206206994597C13D831EC7": {"USDT"},
		"has_contract=true":                  {"USDT", "BTCB", "WBTC"},
		"has_contract=false":                 {"BTC", "ETH", "IRR"},
		"is_token=true&has_contract=true":    {"USDT", "BTCB", "WBTC"},
		"is_token=false&if_fiat=false":       {"BTC", "ETH"},
		"if_fiat=true":                       {"IRR"},
		"created_since=2026-01-04T00:00:00Z": {"IRR", "BTCB", "WBTC"},
		"created_until=2026-01-03T00:00:00Z": {"BTC", "ETH"},
		"updated_since=2026-02-01T00:00:00Z": {"BTC", "USDT"},
		"q=tether":                           {"USDT"},
		"q=تتر":                              {"USDT"},
		"q=bitcoin&is_token=true":            {"BTCB", "WBTC"},
		"symbol=btc&en_name=ether":           {},
	} {
		assert.Equal(t, want, run(t, raw), raw)
	}
}

func TestQuery_SearchRanksMatchingSymbolsFirst(t *testing.T) {
	// BTC is the symbol, BTCB starts with it, and WBTC only contains it.
	assert.Equal(t, []string{"BTC", "BTCB", "WBTC"}, run(t, "q=btc"))
	assert.Equal(t, []string{"BTC", "BTCB", "WBTC"}, run(t, "q=بیت"), "the names match, in the default order")
	assert.Equal(t, []string{"WBTC", "BTCB", "BTC"}, run(t, "q=btc&sort=-symbol"), "a sort replaces the ranking")
}

func TestQuery_Sorts(t *testing.T) {
	for raw, want := range map[string][]string{
		"sort=symbol":      {"BTC", "BTCB", "ETH", "IRR", "USDT", "WBTC"},
		"sort=-symbol":     {"WBTC", "USDT", "IRR", "ETH", "BTCB", "BTC"},
		"sort=en_name":     {"BTC", "BTCB", "ETH", "IRR", "USDT", "WBTC"},
		"sort=-updated_at": {"BTC", "USDT", "WBTC", "BTCB", "IRR", "ETH"},
		// IRR and BTCB were created in the same second: the hk breaks the tie.
		"sort=created_at":  {"BTC", "ETH", "USDT", "IRR", "BTCB", "WBTC"},
		"sort=-created_at": {"WBTC", "BTCB", "IRR", "USDT", "ETH", "BTC"},
	} {
		assert.Equal(t, want, run(t, raw), raw)
	}
	q := parse(t, "")
	q.DefaultDesc = true
	page, err := q.Page(sample, request(t, ""))
	require.NoError(t, err)
	assert.Equal(t, []string{"WBTC", "BTCB", "IRR", "USDT", "ETH", "BTC"}, symbols(page.Items), "the admin default is newest first")
}

// TestQuery_PagesAreStableUnderEverySort walks every order two at a time and
// checks that the pages hold the whole list once, also when a currency of a
// page boundary is removed between pages, as another instance's delete
// would.
func TestQuery_PagesAreStableUnderEverySort(t *testing.T) {
	for _, raw := range []string{"", "q=btc", "q=i", "sort=symbol", "sort=-symbol", "sort=en_name", "sort=-en_name",
		"sort=created_at", "sort=-created_at", "sort=updated_at", "sort=-updated_at"} {
		q := parse(t, raw)
		whole, err := q.Page(sample, request(t, "limit=200"))
		require.NoError(t, err)

		list := append([]models.Currency(nil), sample...)
		var seen []string
		cursor := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 10, raw)
			query := "limit=2"
			if cursor != "" {
				query += "&cursor=" + cursor
			}
			page, err := q.Page(list, request(t, query))
			require.NoError(t, err, raw)
			seen = append(seen, symbols(page.Items)...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
			if pages == 0 {
				// Remove the last currency of the first page.
				last := page.Items[len(page.Items)-1].HK
				list = slicesDelete(list, last)
			}
		}
		assert.Equal(t, symbols(whole.Items), seen, raw)
	}
}

func slicesDelete(list []models.Currency, hk string) []models.Currency {
	out := []models.Currency{}
	for _, c := range list {
		if c.HK != hk {
			out = append(out, c)
		}
	}
	return out
}

func TestQuery_RefusesACursorOfAnotherOrder(t *testing.T) {
	page, err := parse(t, "sort=symbol").Page(sample, request(t, "limit=2"))
	require.NoError(t, err)
	_, err = parse(t, "sort=-symbol").Page(sample, request(t, "limit=2&cursor="+page.NextCursor))
	assert.ErrorIs(t, err, pagination.ErrInvalidCursor)
}

func TestParseQuery_Refusals(t *testing.T) {
	for raw, field := range map[string]string{
		"sybmol=btc":                    "sybmol",
		"status=approved":               "status",
		"is_token=maybe":                "is_token",
		"has_contract=2x":               "has_contract",
		"created_since=yesterday":       "created_since",
		"sort=price":                    "sort",
		"q=" + string(make([]byte, 65)): "q",
		"symbol=ABCDEFGHIJKLMNOP":       "symbol",
	} {
		values, err := url.ParseQuery(raw)
		require.NoError(t, err)
		_, errs := ParseQuery(values)
		assert.Contains(t, errs, field, raw)
	}
	values := url.Values{"status": {"pending,deleted"}}
	q, errs := ParseQuery(values, "status")
	require.Empty(t, errs)
	assert.Equal(t, []string{"pending", "deleted"}, q.Statuses)
	_, errs = ParseQuery(url.Values{"status": {"archived"}}, "status")
	assert.Contains(t, errs, "status")
}

// TestPage_CurrenciesOfOneSecondKeepTheirOrderOfCreation: creation times have
// seconds, so the id orders the currencies created in one second, in both
// directions, whatever their hks.
func TestPage_CurrenciesOfOneSecondKeepTheirOrderOfCreation(t *testing.T) {
	first := currency("ffffffff-0000-0000-0000-000000000000", "BTC", "Bitcoin", "x", "", false, false, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	second := currency("00000000-0000-0000-0000-000000000001", "ETH", "Ethereum", "y", "", false, false, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	first.ID, second.ID = 9, 10
	list := []models.Currency{second, first}
	page, err := Query{}.Page(list, request(t, "limit=10"))
	require.NoError(t, err)
	assert.Equal(t, []string{"BTC", "ETH"}, symbols(page.Items))
	page, err = Query{DefaultDesc: true}.Page(list, request(t, "limit=10"))
	require.NoError(t, err)
	assert.Equal(t, []string{"ETH", "BTC"}, symbols(page.Items))
}

// TestMatch_PreviousSymbolsFindTheCurrency: after a rebrand, a search for
// the symbol before finds the currency, after one whose symbol it is now.
func TestMatch_PreviousSymbolsFindTheCurrency(t *testing.T) {
	pol := currency("00000000-0000-0000-0000-0000000000a1", "POL", "Polygon", "x", "", false, false, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	pol.PreviousSymbols = []string{"MATIC"}
	other := currency("00000000-0000-0000-0000-0000000000a2", "MATICX", "Other", "y", "", false, false, "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z")
	list := []models.Currency{pol, other}
	page, err := parse(t, "symbol=matic").Page(list, request(t, "limit=10"))
	require.NoError(t, err)
	assert.Equal(t, []string{"POL"}, symbols(page.Items))
	page, err = parse(t, "q=matic").Page(list, request(t, "limit=10"))
	require.NoError(t, err)
	assert.Equal(t, []string{"POL", "MATICX"}, symbols(page.Items), "an exact previous symbol ranks with the prefixes")
}
