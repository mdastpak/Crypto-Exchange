package tron

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"google.golang.org/protobuf/encoding/protowire"
)

// The signer builds each transaction itself, in the protobuf of the
// protocol, rather than signing what a node built: a node that lies cannot
// make it sign another transfer than the one it was asked for.

// The types of the contracts of a transaction.
const (
	contractTransfer = 1
	contractTrigger  = 31
)

// Payment is a transfer that the hot wallet pays.
type Payment struct {
	From Address
	To   Address
	// Token is the contract of a TRC20 token; the zero Address for TRX.
	Token Address
	// Amount is in the smallest unit.
	Amount *big.Int
	// FeeLimit is the most TRX, in sun, that a token transfer may burn.
	FeeLimit int64
}

// RefBlock is the recent block that a transaction names, so that it is
// valid on the chain that has the block only, for 65535 blocks.
type RefBlock struct {
	Number int64
	ID     string
}

// Tx is a transaction, signed or not.
type Tx struct {
	Raw       []byte
	Signature []byte
	// Expiration is when the network no longer takes it.
	Expiration time.Time
}

// ID is the hash of the transaction, by which the chain names it.
func (t Tx) ID() string {
	sum := sha256.Sum256(t.Raw)
	return hex.EncodeToString(sum[:])
}

// Lifetime is how long a transaction may wait to be taken: the network
// takes none that expires more than 24 hours out, and a shorter one lets a
// transfer that was never taken be known as such sooner.
const Lifetime = 10 * time.Minute

// ErrPayment is a payment that cannot be built.
var ErrPayment = errors.New("tron: the payment is not valid")

// transferData is the call data of transfer(address,uint256).
func transferData(to Address, amount *big.Int) []byte {
	data := make([]byte, 4+32+32)
	copy(data, []byte{0xa9, 0x05, 0x9c, 0xbb})
	copy(data[4+12:4+32], to[1:])
	amount.FillBytes(data[4+32:])
	return data
}

// Build builds the transaction of p, named by the reference block, at now.
func Build(p Payment, ref RefBlock, now time.Time) (Tx, error) {
	if p.Amount == nil || p.Amount.Sign() <= 0 || p.Amount.BitLen() > 256 {
		return Tx{}, fmt.Errorf("%w: the amount must be positive", ErrPayment)
	}
	if p.From == p.To {
		return Tx{}, fmt.Errorf("%w: the hot wallet does not pay itself", ErrPayment)
	}
	refID, err := hex.DecodeString(ref.ID)
	if err != nil || len(refID) != 32 || ref.Number <= 0 {
		return Tx{}, fmt.Errorf("%w: the reference block is not valid", ErrPayment)
	}
	var contract []byte
	var kind protowire.Number
	var typeURL string
	if p.Token == (Address{}) {
		if !p.Amount.IsInt64() {
			return Tx{}, fmt.Errorf("%w: the amount of TRX is too large", ErrPayment)
		}
		kind, typeURL = contractTransfer, "type.googleapis.com/protocol.TransferContract"
		contract = protowire.AppendTag(contract, 1, protowire.BytesType)
		contract = protowire.AppendBytes(contract, p.From[:])
		contract = protowire.AppendTag(contract, 2, protowire.BytesType)
		contract = protowire.AppendBytes(contract, p.To[:])
		contract = protowire.AppendTag(contract, 3, protowire.VarintType)
		contract = protowire.AppendVarint(contract, uint64(p.Amount.Int64())) //nolint:gosec // G115: positive, checked above.
	} else {
		if p.FeeLimit <= 0 {
			return Tx{}, fmt.Errorf("%w: a token transfer needs a fee limit", ErrPayment)
		}
		kind, typeURL = contractTrigger, "type.googleapis.com/protocol.TriggerSmartContract"
		contract = protowire.AppendTag(contract, 1, protowire.BytesType)
		contract = protowire.AppendBytes(contract, p.From[:])
		contract = protowire.AppendTag(contract, 2, protowire.BytesType)
		contract = protowire.AppendBytes(contract, p.Token[:])
		contract = protowire.AppendTag(contract, 4, protowire.BytesType)
		contract = protowire.AppendBytes(contract, transferData(p.To, p.Amount))
	}
	var anyMsg []byte
	anyMsg = protowire.AppendTag(anyMsg, 1, protowire.BytesType)
	anyMsg = protowire.AppendString(anyMsg, typeURL)
	anyMsg = protowire.AppendTag(anyMsg, 2, protowire.BytesType)
	anyMsg = protowire.AppendBytes(anyMsg, contract)
	var c []byte
	c = protowire.AppendTag(c, 1, protowire.VarintType)
	c = protowire.AppendVarint(c, uint64(kind))
	c = protowire.AppendTag(c, 2, protowire.BytesType)
	c = protowire.AppendBytes(c, anyMsg)

	var number [8]byte
	big.NewInt(ref.Number).FillBytes(number[:])
	expiration := now.Add(Lifetime).Truncate(time.Millisecond)
	var raw []byte
	raw = protowire.AppendTag(raw, 1, protowire.BytesType)
	raw = protowire.AppendBytes(raw, number[6:8])
	raw = protowire.AppendTag(raw, 4, protowire.BytesType)
	raw = protowire.AppendBytes(raw, refID[8:16])
	raw = protowire.AppendTag(raw, 8, protowire.VarintType)
	raw = protowire.AppendVarint(raw, uint64(expiration.UnixMilli()))
	raw = protowire.AppendTag(raw, 11, protowire.BytesType)
	raw = protowire.AppendBytes(raw, c)
	raw = protowire.AppendTag(raw, 14, protowire.VarintType)
	raw = protowire.AppendVarint(raw, uint64(now.UnixMilli()))
	if kind == contractTrigger {
		raw = protowire.AppendTag(raw, 18, protowire.VarintType)
		raw = protowire.AppendVarint(raw, uint64(p.FeeLimit)) //nolint:gosec // G115: positive, checked above.
	}
	return Tx{Raw: raw, Expiration: expiration}, nil
}

// Sign signs t with key: r, s, and the recovery id plus 27.
func (t Tx) Sign(key *secp256k1.PrivateKey) Tx {
	sum := sha256.Sum256(t.Raw)
	compact := ecdsa.SignCompact(key, sum[:], false) // [27+id, r, s]
	t.Signature = append(append([]byte{}, compact[1:]...), compact[0])
	return t
}

// Signer returns the address that signed t.
func (t Tx) Signer() (Address, error) {
	if len(t.Signature) != 65 {
		return Address{}, errors.New("tron: the transaction is not signed")
	}
	sum := sha256.Sum256(t.Raw)
	compact := append([]byte{t.Signature[64]}, t.Signature[:64]...)
	pub, _, err := ecdsa.RecoverCompact(compact, sum[:])
	if err != nil {
		return Address{}, err
	}
	return AddressOf(pub), nil
}

// Encode is the transaction in the protobuf that broadcasthex takes.
func (t Tx) Encode() []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendBytes(out, t.Raw)
	if t.Signature != nil {
		out = protowire.AppendTag(out, 2, protowire.BytesType)
		out = protowire.AppendBytes(out, t.Signature)
	}
	return out
}

// DecodeTx reads a transaction that Encode wrote.
func DecodeTx(b []byte) (Tx, error) {
	var t Tx
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			return Tx{}, errors.New("tron: not an encoded transaction")
		}
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return Tx{}, errors.New("tron: not an encoded transaction")
		}
		b = b[n:]
		switch num {
		case 1:
			t.Raw = append([]byte{}, v...)
		case 2:
			t.Signature = append([]byte{}, v...)
		}
	}
	if t.Raw == nil {
		return Tx{}, errors.New("tron: not an encoded transaction")
	}
	return t, nil
}
