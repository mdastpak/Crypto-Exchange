package price

import (
	"time"

	"github.com/shopspring/decimal"
)

// windows keeps the running sums of pairs of amounts (a numerator and a
// denominator, such as price × size and size for a VWAP) over trailing
// windows of time, so that the average over a window costs O(1) at each
// interval instead of a pass over every trade in it.
//
// The amounts go into buckets of width; a window of span buckets holds the
// buckets whose index is in (now - span, now]. Each window keeps its sums
// and the position of its oldest bucket, and when now moves it subtracts the
// buckets that left it. Decimal sums are exact, so the subtractions never
// drift.
type windows struct {
	width time.Duration
	spans []int64
	// buckets are in ascending index, the oldest first.
	buckets []bucket
	// sums and tails are per window: tails[w] is the position in buckets
	// of the oldest bucket inside window w.
	sums  []sum
	tails []int
	// now is the index of the current bucket.
	now int64
}

type bucket struct {
	index int64
	sum
}

type sum struct {
	num, den decimal.Decimal
}

func (s *sum) add(o sum) { s.num, s.den = s.num.Add(o.num), s.den.Add(o.den) }
func (s *sum) sub(o sum) { s.num, s.den = s.num.Sub(o.num), s.den.Sub(o.den) }

// newWindows returns windows of the lengths, which must be multiples of
// width, in buckets of width.
func newWindows(width time.Duration, lengths []time.Duration) *windows {
	w := &windows{width: width, spans: make([]int64, len(lengths)), sums: make([]sum, len(lengths)), tails: make([]int, len(lengths))}
	for i, l := range lengths {
		w.spans[i] = max(int64(l/width), 1)
	}
	return w
}

func (w *windows) index(at time.Time) int64 { return at.UnixNano() / int64(w.width) }

// longest is the span of the longest window.
func (w *windows) longest() int64 {
	var m int64
	for _, s := range w.spans {
		m = max(m, s)
	}
	return m
}

// add adds the pair observed at. A pair older than the longest window is
// dropped.
func (w *windows) add(at time.Time, num, den decimal.Decimal) {
	i := w.index(at)
	if i <= w.now-w.longest() {
		return
	}
	// Usually the newest bucket, or a new one after it.
	pos := len(w.buckets)
	for pos > 0 && w.buckets[pos-1].index > i {
		pos--
	}
	inserted := pos == 0 || w.buckets[pos-1].index != i
	if inserted {
		w.buckets = append(w.buckets, bucket{})
		copy(w.buckets[pos+1:], w.buckets[pos:])
		w.buckets[pos] = bucket{index: i}
	} else {
		pos--
	}
	v := sum{num, den}
	w.buckets[pos].add(v)
	for k, span := range w.spans {
		inside := i > w.now-span
		if inside {
			w.sums[k].add(v)
		} else if inserted {
			// A bucket before the window moves the window's oldest one.
			w.tails[k]++
		}
	}
}

// advance moves now to at: the buckets that leave a window are subtracted
// from it, and those that left every window are forgotten.
func (w *windows) advance(at time.Time) {
	i := w.index(at)
	if i <= w.now {
		return
	}
	w.now = i
	for k, span := range w.spans {
		for w.tails[k] < len(w.buckets) && w.buckets[w.tails[k]].index <= i-span {
			w.sums[k].sub(w.buckets[w.tails[k]].sum)
			w.tails[k]++
		}
	}
	gone := len(w.buckets)
	for _, t := range w.tails {
		gone = min(gone, t)
	}
	if gone > 0 {
		w.buckets = append(w.buckets[:0], w.buckets[gone:]...)
		for k := range w.tails {
			w.tails[k] -= gone
		}
	}
}

// sum returns the sums of window k.
func (w *windows) sum(k int) sum { return w.sums[k] }
