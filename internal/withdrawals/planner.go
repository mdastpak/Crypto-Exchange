package withdrawals

import (
	"errors"
	"slices"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// RailChain is the rail of a crypto withdrawal: one transfer on chain.
const RailChain = "chain"

// Rail is a bank rail with its limits, as the planner uses them. A nil
// bound is none.
type Rail struct {
	Name               string
	Min, Max, DailyMax *shared.Decimal
	Banks              []string
	Open, Close        int
	HasHours           bool
}

// Part is one transfer of a plan: Amount on Rail, Day days from today.
type Part struct {
	Rail   string
	Amount shared.Decimal
	Day    int
}

// ErrUnplannable is a withdrawal that no rail can pay within the days the
// planner looks ahead.
var ErrUnplannable = errors.New("no rail can pay the withdrawal")

// PlanDays is how many days, today included, a plan may spread over.
const PlanDays = 31

// Used is what a destination already receives on each rail on each day of
// the plan, by day from today and rail: payouts planned, sent, or paid.
type Used map[int]map[string]shared.Decimal

func (u Used) of(day int, rail string) shared.Decimal {
	if d, ok := u[day][rail]; ok {
		return d
	}
	return shared.NewDecimalFromInt64(0)
}

func (u Used) add(day int, rail string, amount shared.Decimal) {
	if u[day] == nil {
		u[day] = map[string]shared.Decimal{}
	}
	u[day][rail] = u.of(day, rail).Add(amount)
}

// Plan splits amount into transfers on rails, in their order of
// preference: each within the bounds of its rail, and, on each day,
// within what remains of the rail's daily limit for the destination after
// used. What today cannot take goes to the next days. A transfer leaves a
// rest that is not below the minimum of its rail when it can, so that the
// rest is payable too.
func Plan(amount shared.Decimal, rails []Rail, used Used) ([]Part, error) {
	if used == nil {
		used = Used{}
	}
	var parts []Part
	remaining := amount
	for day := 0; day < PlanDays && remaining.IsPositive(); day++ {
		for _, r := range rails {
			for remaining.IsPositive() {
				room := remaining
				if r.Max != nil && room.GreaterThan(*r.Max) {
					room = *r.Max
				}
				if r.DailyMax != nil {
					left := r.DailyMax.Sub(used.of(day, r.Name))
					if !left.IsPositive() {
						break
					}
					if room.GreaterThan(left) {
						room = left
					}
				}
				if r.Min != nil {
					if room.LessThan(*r.Min) {
						break
					}
					if rest := remaining.Sub(room); rest.IsPositive() && rest.LessThan(*r.Min) {
						if shrunk := room.Sub(r.Min.Sub(rest)); shrunk.GreaterThanOrEqual(*r.Min) {
							room = shrunk
						}
					}
				}
				parts = append(parts, Part{Rail: r.Name, Amount: room, Day: day})
				used.add(day, r.Name, room)
				remaining = remaining.Sub(room)
			}
		}
	}
	if remaining.IsPositive() {
		return nil, ErrUnplannable
	}
	return parts, nil
}

// RailsFor returns the bank rails that reach a fiat destination, in the
// configured order: card to card for a card; PAYA, SATNA, and Pol for an
// IBAN; and account to account for an account of a bank where the exchange
// holds one. A rail without settings is not used.
func RailsFor(d destinations.Destination, settings config.PayoutsConfig) []Rail {
	reaches := map[destinations.Kind][]string{
		destinations.KindCard:    {config.RailCard},
		destinations.KindIBAN:    {config.RailPaya, config.RailSatna, config.RailPol},
		destinations.KindAccount: {config.RailAccount},
	}[d.Kind]
	var rails []Rail
	for _, name := range settings.Order() {
		c, ok := settings.Rails[name]
		if !ok || !slices.Contains(reaches, name) {
			continue
		}
		if name == config.RailAccount && !slices.Contains(c.Banks, d.Bank) {
			continue
		}
		r := Rail{Name: name, Banks: c.Banks, Min: bound(c.Min), Max: bound(c.Max), DailyMax: bound(c.DailyPerDestination)}
		r.Open, r.Close, r.HasHours = c.Window()
		rails = append(rails, r)
	}
	return rails
}

// bound is a configured bound, validated at startup, or nil for none.
func bound(s string) *shared.Decimal {
	if s == "" {
		return nil
	}
	d, err := shared.NewDecimalFromString(s)
	if err != nil {
		return nil
	}
	return &d
}

// SplitFee splits fee across amounts in proportion to them, rounded down
// to decimals; the last takes the rest, so that the shares sum to fee.
func SplitFee(fee shared.Decimal, amounts []shared.Decimal, decimals int32) []shared.Decimal {
	shares := make([]shared.Decimal, len(amounts))
	if len(amounts) == 0 {
		return shares
	}
	total := shared.NewDecimalFromInt64(0)
	for _, a := range amounts {
		total = total.Add(a)
	}
	left := fee
	for i, a := range amounts {
		if i == len(amounts)-1 {
			shares[i] = left
			break
		}
		share := shared.Decimal(fee.Decimal().Mul(a.Decimal()).Div(total.Decimal()).RoundFloor(decimals))
		shares[i] = share
		left = left.Sub(share)
	}
	return shares
}
