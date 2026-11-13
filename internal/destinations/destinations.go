// Package destinations keeps the withdrawal destinations of users (#160): a
// withdrawal goes only to a destination of the user's book, never to an
// address typed with the withdrawal.
//
// A fiat destination is a card, an IBAN (Sheba), or a bank account of the
// user's own: a provider checks, when it is added, that it belongs to the
// user's verified national ID (#158). A crypto destination is an address
// on a network, valid for its network, that the user added with their
// second factor. A new destination is usable only after a waiting period,
// so that a hijacked session cannot add one and withdraw to it at once.
//
// The numbers and addresses are stored encrypted only (#140) and found by
// blind indexes; the rows are sealed (#139), so that a destination swapped
// in SQL is refused rather than paid.
package destinations

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
)

// Kind is a kind of destination.
type Kind string

// The kinds.
const (
	KindCard    Kind = "card"
	KindIBAN    Kind = "iban"
	KindAccount Kind = "account"
	KindCrypto  Kind = "crypto"
)

// Kinds are the kinds of destination.
var Kinds = []Kind{KindCard, KindIBAN, KindAccount, KindCrypto}

// Fiat reports whether the kind is a card, an IBAN, or an account.
func (k Kind) Fiat() bool { return k == KindCard || k == KindIBAN || k == KindAccount }

// Networks are the networks whose addresses a crypto destination can
// have: those of the asset registry that are on a chain.
var Networks = []string{"bitcoin", "ethereum", "tron"}

// Request is a destination as a user adds it.
type Request struct {
	Kind Kind `json:"kind" enums:"card,iban,account,crypto" example:"iban"`
	// Number is the card number, the IBAN, or the account number, with or
	// without spaces and dashes, in ASCII, Persian, or Arabic digits.
	Number string `json:"number,omitempty" example:"IR820540102680020817909002"`
	// Bank is the bank of an account number, one of Banks. A card or an
	// IBAN names its own.
	Bank string `json:"bank,omitempty" example:"parsian"`
	// Network and Address are a crypto destination's, and Memo its memo
	// or tag where the network has one.
	Network string `json:"network,omitempty" example:"tron"`
	Address string `json:"address,omitempty" example:"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"`
	Memo    string `json:"memo,omitempty"`
	// Label is the user's name for it.
	Label string `json:"label,omitempty" example:"My salary account"`
}

// normalized is a request that passed the checks: the value in its
// canonical form, what its blind index is computed from, and what the user
// is shown.
type normalized struct {
	Kind    Kind
	Value   string
	Bank    string
	Network string
	Memo    string
	Label   string
	// Index is what the blind index is computed from: for an address, its
	// network, its folded form, and its memo, so that the same address in
	// another case, or with the same memo, is the same destination.
	Index  string
	Masked string
}

// ErrInvalid is a request that is not a destination; its message says why.
var ErrInvalid = errors.New("invalid destination")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// normalize checks a request and returns it in its canonical form.
func normalize(r Request) (normalized, error) {
	label := strings.TrimSpace(r.Label)
	if utf8.RuneCountInString(label) > 50 {
		return normalized{}, invalid("a label is at most 50 characters")
	}
	n := normalized{Kind: r.Kind, Label: label}
	var err error
	switch r.Kind {
	case KindCard:
		if n.Value, n.Bank, err = card(r.Number); err != nil {
			return normalized{}, invalid("%v", err)
		}
		n.Masked = n.Value[:6] + "******" + n.Value[12:]
	case KindIBAN:
		if n.Value, n.Bank, err = iban(r.Number); err != nil {
			return normalized{}, invalid("%v", err)
		}
		n.Masked = n.Value[:7] + strings.Repeat("*", 15) + n.Value[22:]
	case KindAccount:
		if n.Value, err = account(r.Number, r.Bank); err != nil {
			return normalized{}, invalid("%v", err)
		}
		n.Bank = r.Bank
		n.Masked = strings.Repeat("*", len(n.Value)-4) + n.Value[len(n.Value)-4:]
	case KindCrypto:
		return normalizeCrypto(n, r)
	default:
		return normalized{}, invalid("the kind is one of %v", Kinds)
	}
	if r.Address != "" || r.Network != "" || r.Memo != "" {
		return normalized{}, invalid("a %s has a number, not an address", r.Kind)
	}
	n.Index = n.Value
	return n, nil
}

func normalizeCrypto(n normalized, r Request) (normalized, error) {
	if r.Number != "" || r.Bank != "" {
		return normalized{}, invalid("a crypto destination has an address, not a number")
	}
	address := strings.TrimSpace(r.Address)
	var err error
	switch r.Network {
	case "ethereum":
		n.Value, err = assetregistry.NormalizeEthereumAddress(address)
	case "tron":
		n.Value, err = address, assetregistry.ValidateTronAddress(address)
	case "bitcoin":
		n.Value, err = assetregistry.ValidateBitcoinAddress(address)
	default:
		return normalized{}, invalid("the network is one of %v", Networks)
	}
	if err != nil {
		return normalized{}, invalid("not a %s address: %v", r.Network, err)
	}
	// None of these networks has a memo; a network that has one, such as
	// TON or XRP, will accept it.
	if r.Memo != "" {
		return normalized{}, invalid("a %s address has no memo", r.Network)
	}
	n.Network = r.Network
	folded := n.Value
	if r.Network == "ethereum" {
		folded = strings.ToLower(folded)
	}
	n.Index = n.Network + "\x00" + folded + "\x00" + n.Memo
	n.Masked = n.Value[:6] + "..." + n.Value[len(n.Value)-6:]
	return n, nil
}

// Destination is a destination of a user's book, as the user sees it: the
// number or address masked.
type Destination struct {
	ID     string `json:"id" format:"uuid"`
	Kind   Kind   `json:"kind" enums:"card,iban,account,crypto"`
	Label  string `json:"label,omitempty"`
	Masked string `json:"masked" example:"IR82054***************9002"`
	// Bank is the bank of a fiat destination, when known.
	Bank string `json:"bank,omitempty" example:"parsian"`
	// Network is the network of a crypto destination.
	Network string `json:"network,omitempty" example:"tron"`
	// UsableFrom is when a withdrawal may use it.
	UsableFrom time.Time `json:"usable_from"`
	CreatedAt  time.Time `json:"created_at"`
	// Version is the version of the row, for the removal.
	Version int64 `json:"version" example:"1"`
}

// Resolved is a destination that a withdrawal may use now, with its number
// or address and memo in the clear, for the payout (#70).
type Resolved struct {
	Destination
	// Value is the card number, the IBAN, the account number, or the
	// address.
	Value string
	Memo  string
}

// Fiat reports whether the destination is a card, an IBAN, or an account.
func (d Destination) Fiat() bool { return d.Kind.Fiat() }
