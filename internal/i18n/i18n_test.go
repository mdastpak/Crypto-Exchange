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

func TestTranslate(t *testing.T) {
	r := MustNew("en", "en", "fa", "fa-AF", "ar")
	fa, _ := r.Lookup("fa")
	en, _ := r.Lookup("en")
	ar, _ := r.Lookup("ar")
	dari, _ := r.Lookup("fa-AF")

	got, ok := r.Translate(fa, "", "Invalid request body")
	assert.True(t, ok)
	assert.Equal(t, "بدنه درخواست نامعتبر است.", got)

	// A message that varies falls back to its code.
	got, ok = r.Translate(fa, "account_frozen", "Some text that varies: 42")
	assert.True(t, ok)
	assert.Equal(t, catalogs["fa"].Codes["account_frozen"], got)

	// Neither: the English message stays.
	got, ok = r.Translate(fa, "", "Nothing like this is translated")
	assert.False(t, ok)
	assert.Equal(t, "Nothing like this is translated", got)

	// English is never translated, and a locale without a catalog stays
	// English, while one of a language with a catalog uses it.
	_, ok = r.Translate(en, "account_frozen", "Invalid request body")
	assert.False(t, ok)
	_, ok = r.Translate(ar, "", "Invalid request body")
	assert.False(t, ok)
	assert.False(t, HasCatalog(ar))
	assert.True(t, HasCatalog(en))
	got, ok = r.Translate(dari, "", "Invalid request body")
	assert.True(t, ok)
	assert.Equal(t, "بدنه درخواست نامعتبر است.", got)
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
		msg, _ := n.Translate("", "Not found")
		c.String(http.StatusOK, n.Locale.Code+" "+msg)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x?lang=fa", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, "fa یافت نشد.", w.Body.String())

	_, ok := FromGin(nil)
	assert.False(t, ok)
	msg, ok := Negotiated{}.Translate("", "Not found")
	assert.False(t, ok)
	assert.Equal(t, "Not found", msg)
}

// TestCatalogs checks every catalog: it parses, is named by a valid tag,
// has no empty translation, keeps the format verbs and the info.* names of
// the English text, and translates only messages and codes that the
// server still writes, so that a reworded message does not leave a stale
// entry behind unnoticed.
func TestCatalogs(t *testing.T) {
	all, err := loadCatalogs()
	require.NoError(t, err)
	require.Contains(t, all, "fa")
	_, hasEnglish := all[English]
	assert.False(t, hasEnglish, "English needs no catalog")

	literals := stringLiterals(t, filepath.Join("..", ".."))
	for code, c := range all {
		require.NotEmpty(t, c.Messages, code)
		for message, translation := range c.Messages {
			assert.NotEmpty(t, strings.TrimSpace(translation), "%s: %q", code, message)
			assert.Equal(t, strings.Count(message, "%"), strings.Count(translation, "%"), "%s: %q", code, message)
			assert.True(t, literals[message], "%s translates %q, which no Go file of the server writes", code, message)
		}
		for errorCode, translation := range c.Codes {
			assert.NotEmpty(t, strings.TrimSpace(translation), "%s: %q", code, errorCode)
			assert.True(t, literals[errorCode], "%s translates code %q, which no Go file of the server defines", code, errorCode)
		}
	}
}

// stringLiterals returns the string literals of the non-test Go files of
// the module under root.
func stringLiterals(t *testing.T, root string) map[string]bool {
	t.Helper()
	literals := map[string]bool{}
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
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					literals[s] = true
				}
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	return literals
}
