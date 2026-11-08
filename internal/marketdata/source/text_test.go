package source

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlainText(t *testing.T) {
	in := `<p>Line <b>one</b></p><p>Line &amp; two &lt;script&gt;alert(1)&lt;/script&gt;</p><!-- note --><img src=x onerror=alert(1)>`
	out := PlainText(in)
	assert.Equal(t, "Line one\nLine & two alert(1)", out)
	assert.NotContains(t, out, "<")
	assert.Equal(t, MaxDescription, len([]rune(PlainText(strings.Repeat("a", MaxDescription+10)))))
	assert.Equal(t, "a b", PlainText("a \x00\x07 b"))
}

func TestHTTPSLink(t *testing.T) {
	assert.Equal(t, "https://example.com/x", HTTPSLink(" https://example.com/x "))
	assert.Equal(t, "https://example.com", HTTPSLink("HTTPS://example.com"))
	for _, bad := range []string{"http://example.com", "javascript:alert(1)", "https://exa mple.com", `https://x.com/"onload`, "https://" + strings.Repeat("a", 500)} {
		assert.Equal(t, "", HTTPSLink(bad), bad)
	}
	assert.Equal(t, "https://b.example", FirstHTTPSLink([]string{"", "ftp://a", "https://b.example"}))
}

func TestNetwork(t *testing.T) {
	assert.Equal(t, "bsc", Network("binance-smart-chain"))
	assert.Equal(t, "bsc", Network("bnb"))
	assert.Equal(t, "ethereum", Network("Ethereum"))
	assert.Equal(t, "some-new-chain", Network("Some New Chain"))
}
