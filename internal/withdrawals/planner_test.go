package withdrawals

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(t *testing.T, s string) shared.Decimal {
	t.Helper()
	d, err := shared.NewDecimalFromString(s)
	require.NoError(t, err)
	return d
}

func ptr(t *testing.T, s string) *shared.Decimal {
	d := dec(t, s)
	return &d
}

// planText is a plan as text: rail amount@day.
func planText(parts []Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Rail + " " + p.Amount.String() + "@" + string(rune('0'+p.Day))
	}
	return out
}

func TestPlan_SplitsAcrossTheLimitsOfTheRails(t *testing.T) {
	// SATNA takes large transfers only; PAYA up to 100; both up to 250 a
	// day for one destination.
	satna := Rail{Name: "satna", Min: ptr(t, "150"), Max: ptr(t, "500"), DailyMax: ptr(t, "250")}
	paya := Rail{Name: "paya", Max: ptr(t, "100"), DailyMax: ptr(t, "250")}

	for name, tc := range map[string]struct {
		amount string
		rails  []Rail
		used   Used
		want   []string
	}{
		"one transfer within a rail": {"80", []Rail{satna, paya}, nil, []string{"paya 80@0"}},
		"the preferred rail first":   {"200", []Rail{satna, paya}, nil, []string{"satna 200@0"}},
		"above a rail's daily limit, the next rail": {"400", []Rail{satna, paya}, nil,
			[]string{"satna 250@0", "paya 100@0", "paya 50@0"}},
		"what today cannot take goes to tomorrow": {"700", []Rail{satna, paya}, nil,
			[]string{"satna 250@0", "paya 100@0", "paya 100@0", "paya 50@0", "satna 200@1"}},
		// SATNA has 50 left today, below its minimum; the 50 left after PAYA
		// is below it too, so it waits for PAYA tomorrow.
		"what the destination received today counts": {"300", []Rail{satna, paya}, Used{0: {"satna": dec(t, "200")}},
			[]string{"paya 100@0", "paya 100@0", "paya 50@0", "paya 50@1"}},
		"a rest below a minimum is avoided": {"320", []Rail{{Name: "satna", Min: ptr(t, "150"), Max: ptr(t, "200")}}, nil,
			[]string{"satna 170@0", "satna 150@0"}},
	} {
		t.Run(name, func(t *testing.T) {
			parts, err := Plan(dec(t, tc.amount), tc.rails, tc.used)
			require.NoError(t, err)
			assert.Equal(t, tc.want, planText(parts))
			total := shared.NewDecimalFromInt64(0)
			for _, p := range parts {
				total = total.Add(p.Amount)
			}
			assert.Equal(t, tc.amount, total.String(), "the parts sum to the amount")
		})
	}

	t.Run("what no rail can pay", func(t *testing.T) {
		_, err := Plan(dec(t, "100"), []Rail{{Name: "satna", Min: ptr(t, "150")}}, nil)
		require.ErrorIs(t, err, ErrUnplannable)
		_, err = Plan(dec(t, "100"), nil, nil)
		require.ErrorIs(t, err, ErrUnplannable)
		_, err = Plan(dec(t, "100000"), []Rail{{Name: "paya", DailyMax: ptr(t, "100")}}, nil)
		require.ErrorIs(t, err, ErrUnplannable, "more than a month of daily limits")
	})
}

func TestRailsFor_TheRailsThatReachADestination(t *testing.T) {
	settings := config.PayoutsConfig{
		RailOrder: []string{"account", "satna", "paya", "card"},
		Rails: map[string]config.RailConfig{
			"account": {Banks: []string{"mellat"}},
			"satna":   {Min: "1500000000", Hours: "08:00-14:00"},
			"paya":    {Max: "1500000000"},
			"card":    {Max: "100000000"},
		},
	}
	names := func(rails []Rail) []string {
		var out []string
		for _, r := range rails {
			out = append(out, r.Name)
		}
		return out
	}
	assert.Equal(t, []string{"satna", "paya"}, names(RailsFor(destinations.Destination{Kind: destinations.KindIBAN}, settings)),
		"Pol has no settings")
	assert.Equal(t, []string{"card"}, names(RailsFor(destinations.Destination{Kind: destinations.KindCard}, settings)))
	assert.Equal(t, []string{"account"}, names(RailsFor(destinations.Destination{Kind: destinations.KindAccount, Bank: "mellat"}, settings)))
	assert.Empty(t, RailsFor(destinations.Destination{Kind: destinations.KindAccount, Bank: "parsian"}, settings),
		"no account of the exchange at that bank")
	satna := RailsFor(destinations.Destination{Kind: destinations.KindIBAN}, settings)[0]
	assert.True(t, satna.HasHours)
	assert.Equal(t, 8*60, satna.Open)
	assert.Equal(t, 14*60, satna.Close)
}

func TestSplitFee_SharesSumToTheFee(t *testing.T) {
	amounts := []shared.Decimal{dec(t, "250"), dec(t, "100"), dec(t, "50")}
	shares := SplitFee(dec(t, "10"), amounts, 0)
	assert.Equal(t, []string{"6", "2", "2"}, []string{shares[0].String(), shares[1].String(), shares[2].String()})
	shares = SplitFee(dec(t, "1"), []shared.Decimal{dec(t, "1"), dec(t, "1"), dec(t, "1")}, 6)
	assert.Equal(t, "0.333333", shares[0].String())
	assert.Equal(t, "0.333334", shares[2].String())
	assert.Equal(t, "0", SplitFee(dec(t, "0"), amounts, 0)[2].String())
}
