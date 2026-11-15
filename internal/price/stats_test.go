package price

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func decimals(values ...string) []decimal.Decimal {
	out := make([]decimal.Decimal, len(values))
	for i, v := range values {
		out[i] = d(v)
	}
	return out
}

func TestMedian(t *testing.T) {
	assert.Equal(t, "3", median(decimals("5", "1", "3")).String())
	assert.Equal(t, "2.5", median(decimals("4", "1", "3", "2")).String())
	assert.Equal(t, "7", median(decimals("7")).String())
	in := decimals("3", "1", "2")
	median(in)
	assert.Equal(t, "3", in[0].String(), "the values are not reordered")
}

func TestOutliers_MAD(t *testing.T) {
	k, floor := d("3"), d("0.001")
	// Median 100, deviations 0, 1, 1, 20: MAD 1, limit 3.
	assert.Equal(t, []bool{false, false, false, true}, outliers(decimals("100", "101", "99", "120"), k, floor))
	// Within 3 MADs.
	assert.Equal(t, []bool{false, false, false, false}, outliers(decimals("100", "102", "98", "103"), k, floor))
	// A MAD of 0: the floor (0.1 % of 100 = 0.1) keeps the noise in, and
	// the far one out.
	assert.Equal(t, []bool{false, false, false, false, true}, outliers(decimals("100", "100", "100", "100.05", "101"), k, floor))
	// With no floor, any deviation past a MAD of 0 is out.
	assert.Equal(t, []bool{false, false, true}, outliers(decimals("100", "100", "100.05"), k, decimal.Zero))
	// Two providers are each as far from their median: neither is out.
	assert.Equal(t, []bool{false, false}, outliers(decimals("100", "200"), k, floor))
	assert.Empty(t, outliers(nil, k, floor))
}

func TestAverage(t *testing.T) {
	v, ok := average(sum{num: d("10"), den: d("3")})
	assert.True(t, ok)
	assert.Equal(t, "3.333333333333333333", v.String(), "rounded to 18 places")
	_, ok = average(sum{num: d("1"), den: decimal.Zero})
	assert.False(t, ok)
}

// A fixed fixture: trades into windows of 1 s and 5 s, in buckets of
// 100 ms.
func TestWindows_VWAPFixture(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	w := newWindows(100*time.Millisecond, []time.Duration{time.Second, 5 * time.Second})
	trade := func(at time.Duration, price, size string) {
		w.add(t0.Add(at), d(price).Mul(d(size)), d(size))
	}
	trade(0, "100", "1")
	trade(2*time.Second, "110", "3")
	trade(4500*time.Millisecond, "120", "1")
	w.advance(t0.Add(4900 * time.Millisecond))

	oneSecond, _ := average(w.sum(0))
	fiveSeconds, _ := average(w.sum(1))
	assert.Equal(t, "120", oneSecond.String(), "only the trade at 4.5 s is within the last second")
	// (100×1 + 110×3 + 120×1) / 5 = 110
	assert.Equal(t, "110", fiveSeconds.String())

	// At 5.2 s the trade at 0 leaves the 5 s window: (330 + 120) / 4.
	w.advance(t0.Add(5200 * time.Millisecond))
	fiveSeconds, _ = average(w.sum(1))
	assert.Equal(t, "112.5", fiveSeconds.String())
	// At 6 s the 1 s window is empty.
	w.advance(t0.Add(6 * time.Second))
	_, ok := average(w.sum(0))
	assert.False(t, ok)
	// A late trade inside the 5 s window still counts.
	trade(3*time.Second, "100", "4")
	fiveSeconds, _ = average(w.sum(1))
	// (110×3 + 120×1 + 100×4) / 8 = 106.25
	assert.Equal(t, "106.25", fiveSeconds.String())
	// A trade older than every window is dropped.
	trade(-time.Hour, "1", "1000")
	fiveSeconds, _ = average(w.sum(1))
	assert.Equal(t, "106.25", fiveSeconds.String())
}

// The running sums match a brute-force pass over random trades, in random
// order and with gaps.
func TestWindows_MatchBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	width := 100 * time.Millisecond
	lengths := []time.Duration{time.Second, 5 * time.Second, 60 * time.Second}
	w := newWindows(width, lengths)
	type trade struct {
		at          time.Time
		price, size decimal.Decimal
	}
	var trades []trade
	t0 := time.Unix(1_700_000_000, 0)
	now := t0
	for step := 0; step < 3000; step++ {
		now = now.Add(time.Duration(r.IntN(300)) * time.Millisecond)
		for range r.IntN(4) {
			// Up to 2 s late.
			at := now.Add(-time.Duration(r.IntN(2000)) * time.Millisecond)
			tr := trade{at, decimal.NewFromInt(int64(100 + r.IntN(50))), decimal.New(int64(1+r.IntN(1000)), -3)}
			trades = append(trades, tr)
			w.add(tr.at, tr.price.Mul(tr.size), tr.size)
		}
		w.advance(now)
		for k, l := range lengths {
			var want sum
			nowIndex := now.UnixNano() / int64(width)
			for _, tr := range trades {
				i := tr.at.UnixNano() / int64(width)
				if i > nowIndex-int64(l/width) && i <= nowIndex {
					want.add(sum{tr.price.Mul(tr.size), tr.size})
				}
			}
			got := w.sum(k)
			if !got.num.Equal(want.num) || !got.den.Equal(want.den) {
				t.Fatalf("step %d, window %s: got %s/%s, want %s/%s", step, l, got.num, got.den, want.num, want.den)
			}
		}
	}
	assert.Less(t, len(w.buckets), 700, "the buckets older than every window are forgotten")
}
