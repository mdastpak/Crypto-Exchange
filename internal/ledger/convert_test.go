package ledger

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestSplit(t *testing.T) {
	d := decimal.RequireFromString
	for _, c := range []struct {
		amount, ratio string
		precision     int32
		credit, dust  string
	}{
		{"150.5", "0.01", 2, "1.5", "0.005"},
		{"1", "24000", 18, "24000", "0"},
		{"0.1", "0.01", 2, "0", "0.001"},
		{"3", "0.433350", 6, "1.30005", "0"},
		{"1", "0.333333333333333333333", 6, "0.333333", "0.000000333333333333"},
		{"5", "1", 40, "5", "0"},
	} {
		credit, dust := Split(d(c.amount), d(c.ratio), c.precision)
		assert.True(t, d(c.credit).Equal(credit), "%v: credit %s", c, credit)
		assert.True(t, d(c.dust).Equal(dust), "%v: dust %s", c, dust)
	}
}
