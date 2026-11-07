package clientip

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKey(t *testing.T) {
	for _, tc := range []struct {
		addr   string
		prefix int
		want   string
	}{
		{"203.0.113.7", 64, "203.0.113.7"},
		{"::ffff:203.0.113.7", 64, "203.0.113.7"},
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", 64, "2001:db8:1:2::/64"},
		{"2001:db8:1:2::1", 64, "2001:db8:1:2::/64"},
		{"2001:db8:1:2:aaaa::1", 56, "2001:db8:1::/56"},
		{"2001:db8:1:2:aaaa::1", 128, "2001:db8:1:2:aaaa::1/128"},
		{"2001:db8:1:2:aaaa::1", 0, "2001:db8:1:2::/64"},
		{"2001:db8:1:2:aaaa::1", 200, "2001:db8:1:2::/64"},
		{"fe80::1%eth0", 64, "fe80::/64"},
		{"::1", 64, "::/64"},
		{"not an address", 64, "not an address"},
		{"", 64, ""},
	} {
		assert.Equal(t, tc.want, Key(tc.addr, tc.prefix), "%s /%d", tc.addr, tc.prefix)
	}
}

func TestKey_SameSubscriberSharesAKey(t *testing.T) {
	a := Key("2001:db8:abcd:12:1::1", 64)
	b := Key("2001:db8:abcd:12:ffff:ffff:ffff:ffff", 64)
	c := Key("2001:db8:abcd:13::1", 64)
	assert.Equal(t, a, b, "the same /64")
	assert.NotEqual(t, a, c, "another /64")
}
