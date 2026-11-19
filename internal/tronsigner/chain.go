package tronsigner

import (
	"context"
	"log"
	"time"
)

// The signer decides where a payment is from the chain's view alone, so
// that it never signs a payment again that the network took (#550):
//
//   - final once a solidified block holds one of the transactions ever
//     signed for its key, which no reorg can undo;
//   - pending while any of them is in a block that is not solidified yet,
//     or the node looks stale;
//   - never taken only when the solidified head's own time is past the
//     expiration of the newest one, by expiryMargin, and none of them is in
//     any block: an expired transaction can no longer be taken, and the
//     solidified chain has passed the time when it could.
//
// The signer's clock only tells whether the node is stale: a node whose
// head is older than stale_after_seconds answers "not found" for what it
// has not read yet, and nothing is decided on it.

// expiryMargin is how far the solidified head must be past a
// transaction's expiration before it is known never to be taken.
const expiryMargin = time.Minute

// The states of the transactions of a key.
const (
	txPending = iota
	txFinal
	txFailed
	txExpired
)

// view is where the transactions of a key are.
type view struct {
	state int
	// id is the transaction that settled the key: final or failed.
	id     string
	reason string
	// inBlock reports one of them in a block that is not solidified yet.
	inBlock bool
}

// settle tells where the transactions ever signed for a key are, the
// newest last, which expires at expiration.
func (s *Signer) settle(ctx context.Context, ids []string, expiration time.Time) (view, error) {
	var failed *view
	for _, id := range ids {
		r, err := s.node.SolidReceipt(ctx, id)
		if err != nil {
			return view{}, err
		}
		if !r.Found {
			continue
		}
		if r.Success {
			return view{state: txFinal, id: id}, nil
		}
		if failed == nil {
			failed = &view{state: txFailed, id: id, reason: r.Result}
		}
	}
	if failed != nil {
		return *failed, nil
	}
	for _, id := range ids {
		r, err := s.node.Receipt(ctx, id)
		if err != nil {
			return view{}, err
		}
		if r.Found {
			return view{state: txPending, inBlock: true}, nil
		}
	}
	head, err := s.node.Head(ctx)
	if err != nil {
		return view{}, err
	}
	if behind := s.now().Sub(head.Time); behind > s.cfg.staleAfter() {
		log.Printf("tron-signer: the node's head %d is %s old: nothing is decided until it catches up", head.Number, behind.Round(time.Second))
		return view{state: txPending}, nil
	}
	solid, err := s.node.SolidHead(ctx)
	if err != nil {
		return view{}, err
	}
	if solid.Time.After(expiration.Add(expiryMargin)) {
		return view{state: txExpired}, nil
	}
	return view{state: txPending}, nil
}
