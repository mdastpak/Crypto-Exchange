package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RefusesInvalidLists(t *testing.T) {
	for name, tc := range map[string]struct {
		def   string
		codes []string
		want  string
	}{
		"no English":          {"fa", []string{"fa"}, `must include "en"`},
		"listed twice":        {"en", []string{"en", "fa", "fa"}, `"fa" is listed twice`},
		"twice after parsing": {"en", []string{"en", "fa_IR", "fa-ir"}, `"fa-IR" is listed twice`},
		"not a tag":           {"en", []string{"en", "not a tag"}, "not a BCP 47 language tag"},
		"empty code":          {"en", []string{"en", ""}, "is empty"},
		"default not listed":  {"de", []string{"en", "fa"}, `default locale "de" is not one of the locales (en, fa)`},
		"default not a tag":   {"%%", []string{"en"}, "default locale"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(tc.def, tc.codes)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNew_DirectionAndNamesComeFromTheLanguage(t *testing.T) {
	r := MustNew("fa", "en", "fa", "ar", "he", "ckb", "az", "az-Arab", "tr")
	want := map[string][3]string{
		"en":      {"English", "English", LTR},
		"fa":      {"Persian", "فارسی", RTL},
		"ar":      {"Arabic", "العربية", RTL},
		"he":      {"Hebrew", "עברית", RTL},
		"ckb":     {"Central Kurdish", "کوردیی ناوەندی", RTL},
		"az":      {"Azerbaijani", "azərbaycan", LTR},
		"az-Arab": {"", "", RTL},
		"tr":      {"Turkish", "Türkçe", LTR},
	}
	for _, l := range r.Locales() {
		w, ok := want[l.Code]
		require.True(t, ok, l.Code)
		assert.Equal(t, w[2], l.Dir, l.Code)
		if w[0] != "" {
			assert.Equal(t, w[0], l.Name, l.Code)
			assert.Equal(t, w[1], l.NativeName, l.Code)
		}
		assert.NotEmpty(t, l.Name, l.Code)
		assert.NotEmpty(t, l.NativeName, l.Code)
	}
	assert.Equal(t, "fa", r.Default().Code)
	assert.Equal(t, []string{"en", "fa", "ar", "he", "ckb", "az", "az-Arab", "tr"}, r.Codes())
}

func TestLookup(t *testing.T) {
	r := MustNew("en", "en", "fa", "zh-Hant")
	for code, want := range map[string]string{
		"fa": "fa", "FA": "fa", "fa-IR": "fa", "fa_IR": "fa", " fa ": "fa",
		"en-US": "en", "zh-Hant-TW": "zh-Hant", "zh": "zh-Hant",
	} {
		l, ok := r.Lookup(code)
		assert.True(t, ok, code)
		assert.Equal(t, want, l.Code, code)
	}
	for _, code := range []string{"", "de", "xx-invalid-tag-%", "und"} {
		_, ok := r.Lookup(code)
		assert.False(t, ok, code)
	}
}

func TestMatch_AcceptLanguage(t *testing.T) {
	r := MustNew("en", "en", "fa", "az")
	for header, want := range map[string]string{
		"fa-IR,fa;q=0.9,en;q=0.8": "fa",
		"de-DE,de;q=0.9,fa;q=0.5": "fa",
		"en-GB":                   "en",
		"fa-AF":                   "fa",
	} {
		l, ok := r.Match(header)
		assert.True(t, ok, header)
		assert.Equal(t, want, l.Code, header)
	}
	// Turkish is related to Azerbaijani, not close enough to show it.
	for _, header := range []string{"", "de", "tr-TR", ";;;", "*"} {
		_, ok := r.Match(header)
		assert.False(t, ok, header)
	}
}

func TestNegotiate_Order(t *testing.T) {
	r := MustNew("en", "en", "fa", "ar")
	request := func(query, header, cookie, accept string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/x"+query, nil)
		if header != "" {
			req.Header.Set(Header, header)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: Cookie, Value: cookie})
		}
		if accept != "" {
			req.Header.Set("Accept-Language", accept)
		}
		return req
	}
	for name, tc := range map[string]struct {
		req            *http.Request
		locale, source string
		explicit       bool
	}{
		"query first":              {request("?lang=ar", "fa", "fa", "fa"), "ar", SourceQuery, true},
		"header before cookie":     {request("", "ar", "fa", "fa"), "ar", SourceHeader, true},
		"cookie before browser":    {request("", "", "ar", "fa"), "ar", SourceCookie, true},
		"browser":                  {request("", "", "", "fa-IR"), "fa", SourceAcceptLanguage, false},
		"default":                  {request("", "", "", ""), "en", SourceDefault, false},
		"unsupported are skipped":  {request("?lang=de", "xx", "ja", "fa"), "fa", SourceAcceptLanguage, false},
		"unsupported then default": {request("?lang=de", "", "", "ja"), "en", SourceDefault, false},
	} {
		t.Run(name, func(t *testing.T) {
			n := r.Negotiate(tc.req)
			assert.Equal(t, tc.locale, n.Locale.Code)
			assert.Equal(t, tc.source, n.Source)
			assert.Equal(t, tc.explicit, n.Explicit())
		})
	}
}

func TestRender(t *testing.T) {
	r := MustNew("en", "en", "fa", "fa-AF", "ar")
	fa, _ := r.Lookup("fa")
	en, _ := r.Lookup("en")
	ar, _ := r.Lookup("ar")
	dari, _ := r.Lookup("fa-AF")

	got, lang := r.Render(fa, M("INVALID_REQUEST_BODY"))
	assert.Equal(t, "fa", lang)
	assert.Equal(t, "بدنه درخواست نامعتبر است.", got)
	got, lang = r.Render(en, M("INVALID_REQUEST_BODY"))
	assert.Equal(t, "en", lang)
	assert.Equal(t, "Invalid request body", got)

	// A locale without a catalog answers in English, and one of a language
	// with a catalog uses it.
	got, lang = r.Render(ar, M("INVALID_REQUEST_BODY"))
	assert.Equal(t, "en", lang)
	assert.Equal(t, "Invalid request body", got)
	assert.False(t, HasCatalog(ar))
	assert.True(t, HasCatalog(en))
	got, lang = r.Render(dari, M("INVALID_REQUEST_BODY"))
	assert.Equal(t, "fa-AF", lang)
	assert.Equal(t, "بدنه درخواست نامعتبر است.", got)

	// A key that no catalog has renders as itself, never as nothing.
	got, lang = r.Render(fa, M("NO_SUCH_KEY_ANYWHERE"))
	assert.Equal(t, "en", lang)
	assert.Equal(t, "NO_SUCH_KEY_ANYWHERE", got)
}

func TestRender_Parameters(t *testing.T) {
	r := MustNew("en", "en", "fa")
	fa, _ := r.Lookup("fa")
	en, _ := r.Lookup("en")
	m := M("CURRENCY_OPERATION_UNAVAILABLE", A("operation", Key("CAPABILITY_BUY")), A("currency", "BTC"))

	// A nested key is translated, and a literal is inserted as it is: in
	// RTL text inside isolates, so that BTC/USDT or an amount keeps its
	// own direction.
	got, _ := r.Render(fa, m)
	assert.Equal(t, "خرید ارز \u2068BTC\u2069 در حال حاضر در دسترس نیست.", got)
	got, _ = r.Render(en, m)
	assert.Equal(t, "The buying of BTC is not available now.", got)
	assert.Equal(t, got, m.String())

	// A nested message, and a parameter without a value.
	nested := M("CODES_CANNOT_BE_SENT_BY", A("channel", M("CHANNEL_MOBILE")))
	got, _ = r.Render(fa, nested)
	assert.Equal(t, "امکان ارسال کد از راه شماره\u200cی موبایل وجود ندارد.", got)
	assert.Equal(t, "Codes cannot be sent by .", M("CODES_CANNOT_BE_SENT_BY").String())
	assert.True(t, Message{}.IsZero())
}

func TestParseTemplate(t *testing.T) {
	tpl, err := parseTemplate("{{literal}} {name} and {other_2}")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"name": true, "other_2": true}, tpl.params())
	assert.Equal(t, "{literal} a and b", tpl.execute(map[string]string{"name": "a", "other_2": "b"}))

	for _, text := range []string{"", "  ", "{open", "close}", "{Upper}", "{with space}", "{}"} {
		_, err := parseTemplate(text)
		assert.Error(t, err, "%q", text)
	}
}

func TestMiddleware_KeepsTheLocaleOnTheRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := MustNew("en", "en", "fa")
	engine := gin.New()
	engine.Use(Middleware(r))
	engine.GET("/x", func(c *gin.Context) {
		n, ok := FromGin(c)
		require.True(t, ok)
		fromCtx, ok := FromContext(c.Request.Context())
		require.True(t, ok)
		assert.Equal(t, n.Locale.Code, fromCtx.Locale.Code)
		msg, _ := n.Render(M("NOT_FOUND"))
		c.String(http.StatusOK, n.Locale.Code+" "+msg)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?lang=fa", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, "fa یافت نشد.", w.Body.String())

	_, ok := FromGin(nil)
	assert.False(t, ok)
	msg, lang := Negotiated{}.Render(M("NOT_FOUND"))
	assert.Equal(t, "en", lang)
	assert.Equal(t, "Not found", msg)
}

// TestCatalogs checks every catalog: it parses, is named by a valid tag,
// defines only keys of the English catalog with the same parameters, and
// the fa catalog translates every key. It also checks that every key the
// server sends is defined, and that every defined key is still sent, so
// that a removed message does not leave a stale entry behind.
func TestCatalogs(t *testing.T) {
	all, err := loadCatalogs()
	require.NoError(t, err)
	english := all[English]
	require.NotEmpty(t, english)
	require.Contains(t, all, "fa")

	for code, c := range all {
		for key, tpl := range c {
			source, ok := english[key]
			if !assert.True(t, ok, "%s.json has %s, which en.json lacks", code, key) {
				continue
			}
			assert.Equal(t, source.params(), tpl.params(), "%s.json: %s has other parameters than in en.json", code, key)
		}
	}
	for key := range english {
		assert.Contains(t, all["fa"], key, "fa.json lacks %s", key)
	}

	used, families := messageKeys(t, filepath.Join("..", ".."))
	for key, where := range used {
		assert.Contains(t, english, key, "%s sends %s, which en.json lacks", where, key)
	}
	for key := range english {
		if _, ok := used[key]; ok {
			continue
		}
		family := false
		for _, prefix := range families {
			family = family || strings.HasPrefix(string(key), prefix)
		}
		assert.True(t, family, "en.json has %s, which no Go file of the server sends", key)
	}
}

// messageKeys returns the keys that the non-test Go files of the module
// under root name in i18n.M and i18n.Key, with the file of each, and the
// prefixes of the keys that they build, such as "CAPABILITY_" in
// i18n.Key("CAPABILITY_" + ...). The packages that build keys test that
// each of their values has one.
func messageKeys(t *testing.T, root string) (map[Key]string, []string) {
	t.Helper()
	used := map[Key]string{}
	var prefixes []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "docs" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 || !isKeyFunc(call.Fun) {
				return true
			}
			switch arg := call.Args[0].(type) {
			case *ast.BasicLit:
				if s, err := strconv.Unquote(arg.Value); err == nil {
					used[Key(s)] = path
				}
			case *ast.BinaryExpr:
				if lit, ok := arg.X.(*ast.BasicLit); ok {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						prefixes = append(prefixes, s)
					}
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	return used, prefixes
}

// isKeyFunc reports a call of M or Key, of this package or as i18n.M.
func isKeyFunc(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "M" || f.Name == "Key"
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		return ok && pkg.Name == "i18n" && (f.Sel.Name == "M" || f.Sel.Name == "Key")
	}
	return false
}
