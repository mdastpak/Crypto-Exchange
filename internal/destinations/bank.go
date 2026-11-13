package destinations

import (
	"errors"
	"slices"
	"strings"
)

// Banks are the Iranian banks that a fiat destination can name, by the
// slug that the destinations record. The payouts prefer an
// account-to-account transfer when the destination's bank is one where the
// payment provider holds an account (#70, #74).
var Banks = []string{
	"ayandeh", "day", "eghtesad_novin", "gardeshgari", "iran_zamin", "karafarin", "keshavarzi",
	"khavarmianeh", "maskan", "melal", "mellat", "melli", "mehr_iran", "parsian", "pasargad", "post",
	"refah", "resalat", "saderat", "saman", "sanat_madan", "sarmayeh", "sepah", "shahr", "sina",
	"tejarat", "tosee_saderat", "tosee_taavon",
}

// ibanBanks maps the bank code of an Iranian IBAN, the three digits after
// IR and the check digits, to its bank.
var ibanBanks = map[string]string{
	"011": "sanat_madan", "012": "mellat", "013": "refah", "014": "maskan", "015": "sepah",
	"016": "keshavarzi", "017": "melli", "018": "tejarat", "019": "saderat", "020": "tosee_saderat",
	"021": "post", "022": "tosee_taavon", "053": "karafarin", "054": "parsian", "055": "eghtesad_novin",
	"056": "saman", "057": "pasargad", "058": "sarmayeh", "059": "sina", "060": "mehr_iran",
	"061": "shahr", "062": "ayandeh", "064": "gardeshgari", "066": "day", "069": "iran_zamin",
	"070": "resalat", "075": "melal", "078": "khavarmianeh",
}

// cardBanks maps the first six digits of an Iranian card, its issuer
// prefix, to its bank. A card whose prefix is not listed is still
// accepted; its bank is unknown.
var cardBanks = map[string]string{
	"603799": "melli", "589210": "sepah", "627648": "tosee_saderat", "627961": "sanat_madan",
	"603770": "keshavarzi", "628023": "maskan", "627760": "post", "502908": "tosee_taavon",
	"627412": "eghtesad_novin", "622106": "parsian", "502229": "pasargad", "627488": "karafarin",
	"621986": "saman", "639346": "sina", "639607": "sarmayeh", "636214": "ayandeh", "502806": "shahr",
	"502938": "day", "603769": "saderat", "610433": "mellat", "627353": "tejarat", "585983": "tejarat",
	"589463": "refah", "505785": "iran_zamin", "504172": "resalat", "606373": "mehr_iran",
	"505416": "gardeshgari", "606256": "melal", "585947": "khavarmianeh",
}

// digits returns s with Persian and Arabic digits written as ASCII digits,
// and with the spaces, dashes, and invisible marks that people type or
// paste between groups of digits left out.
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '۰' && r <= '۹':
			b.WriteRune('0' + (r - '۰'))
		case r >= '٠' && r <= '٩':
			b.WriteRune('0' + (r - '٠'))
		case r == ' ' || r == '-' || r == 0x200c || r == 0x200e || r == 0x200f || r == 0xa0:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func allDigits(s string) bool {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// card checks a card number: 16 digits that pass the Luhn check. It
// returns the number and its bank, or "" for a prefix it does not know.
func card(s string) (number, bank string, err error) {
	number = digits(s)
	if len(number) != 16 || !allDigits(number) {
		return "", "", errors.New("a card number is 16 digits")
	}
	sum := 0
	for i := range 16 {
		d := int(number[15-i] - '0')
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	if sum%10 != 0 {
		return "", "", errors.New("the card number's check digit does not match")
	}
	return number, cardBanks[number[:6]], nil
}

// iban checks an Iranian IBAN (Sheba): IR, two check digits, and 22 more
// digits, whose ISO 7064 check (mod 97) is 1. IR may be left out. It
// returns the IBAN and its bank, or "" for a code it does not know.
func iban(s string) (number, bank string, err error) {
	number = strings.ToUpper(digits(s))
	if allDigits(number) && len(number) == 24 {
		number = "IR" + number
	}
	if len(number) != 26 || !strings.HasPrefix(number, "IR") || !allDigits(number[2:]) {
		return "", "", errors.New("an IBAN is IR and 24 digits")
	}
	// The country and the check digits go to the end, and I and R are 18
	// and 27.
	rearranged := number[4:] + "1827" + number[2:4]
	rem := 0
	for _, c := range []byte(rearranged) {
		rem = (rem*10 + int(c-'0')) % 97
	}
	if rem != 1 {
		return "", "", errors.New("the IBAN's check digits do not match")
	}
	return number, ibanBanks[number[4:7]], nil
}

// account checks a bank account number, which needs its bank: 5 to 26
// digits.
func account(s, bank string) (string, error) {
	number := digits(s)
	if len(number) < 5 || len(number) > 26 || !allDigits(number) {
		return "", errors.New("an account number is 5 to 26 digits")
	}
	if !slices.Contains(Banks, bank) {
		return "", errors.New("an account number needs its bank, one of the banks listed")
	}
	return number, nil
}
