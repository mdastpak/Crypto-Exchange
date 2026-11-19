package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/tron"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// depositWatch is the watcher of a network, and how often it runs.
type depositWatch struct {
	watcher  *deposits.Watcher
	interval time.Duration
}

// useDeposits gives the users their deposit addresses (#68) on each enabled
// custody network with an extended public key: TRON for now. An xpub that
// does not parse, or an extended private key, stops the server; a network
// without one takes no deposits, and says so. It returns the watcher of
// each network, which finds the deposits to the addresses, evaluated by
// the AML rules when they are on.
func useDeposits(h *InfraHandlers, db *sql.DB, settings *config.Configuration, rules *amlSetup) ([]depositWatch, error) {
	levels, ok := h.User.Verification.(verification.Store)
	if !ok {
		return nil, nil
	}
	networks := map[string]deposits.Deriver{}
	var watches []depositWatch
	for i, p := range settings.Custody.Providers {
		if !p.Enabled || p.Network != "tron" {
			continue
		}
		if p.XPub == "" {
			log.Printf("custody.providers[%d] (tron) has no xpub: the users get no TRON deposit address", i)
			continue
		}
		x, err := tron.ParseXPub(p.XPub)
		if err != nil {
			return nil, fmt.Errorf("custody.providers[%d].xpub: %w", i, err)
		}
		node := nodeURL(p.RPCEndpoints)
		if node == "" {
			return nil, fmt.Errorf("custody.providers[%d].rpc_endpoints: a TRON node is required with the xpub, to watch the deposits", i)
		}
		networks["tron"] = x
		w := &deposits.Watcher{DB: db, Network: "tron",
			Chain:         tronChain{tron.Node{BaseURL: node, APIKey: p.APIKey, Client: &http.Client{Timeout: 20 * time.Second}}},
			Confirmations: int64(orDefault(p.ConfirmationsRequired, 19)), ReorgDepth: int64(orDefault(p.ReorgDepth, 19)),
			Start: p.StartBlock, MaxBlocks: 200, Ignore: map[string]bool{}}
		for _, a := range []string{p.HotWalletAddress, p.ColdWalletAddress} {
			if a != "" {
				w.Ignore[a] = true
			}
		}
		if rules != nil {
			w.AML = rules.engine
		}
		watches = append(watches, depositWatch{watcher: w, interval: time.Duration(orDefault(p.BlockPollInterval, 3)) * time.Second})
	}
	if len(networks) == 0 {
		return nil, nil
	}
	h.User.Deposits = deposits.Service{DB: db, Networks: networks, Levels: levels}
	h.Admin.Deposits = deposits.Reviews{DB: db}
	return watches, nil
}

// nodeURL is the first endpoint of the HTTP API of a TRON node: not the
// JSON-RPC one.
func nodeURL(endpoints []string) string {
	for _, e := range endpoints {
		if !strings.HasSuffix(strings.TrimRight(e, "/"), "/jsonrpc") {
			return e
		}
	}
	return ""
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// tronChain reads TRON for the deposit watcher.
type tronChain struct{ node tron.Node }

func blockOf(b tron.Block) deposits.Block {
	out := deposits.Block{Number: b.Number, ID: b.ID, ParentID: b.ParentID}
	for _, t := range b.Transfers {
		token := ""
		if t.Token != (tron.Address{}) {
			token = t.Token.String()
		}
		out.Transfers = append(out.Transfers, deposits.Transfer{TxID: t.TxID, Index: t.Index, Token: token,
			From: t.From.String(), To: t.To.String(), Amount: t.Amount})
	}
	return out
}

func (c tronChain) Head(ctx context.Context) (deposits.Block, error) {
	b, err := c.node.Head(ctx)
	return blockOf(b), err
}

func (c tronChain) Block(ctx context.Context, n int64) (deposits.Block, error) {
	b, err := c.node.Block(ctx, n)
	return blockOf(b), err
}

func (c tronChain) TxBlock(ctx context.Context, txID string) (int64, bool, error) {
	return c.node.TxBlock(ctx, txID)
}

// startDepositWatch runs the watcher of a network every interval while
// neither the scheduler nor the deposits are paused. Each instance runs
// it; the cursor of the network lets one record each block.
func startDepositWatch(d depositWatch, paused func() bool) worker {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if paused() {
				continue
			}
			runCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
			res, err := d.watcher.Run(runCtx)
			stop()
			if err != nil {
				log.Printf("Error watching the deposits of %s: %v", d.watcher.Network, err)
			}
			if res.Detected+res.Credited+res.Held+res.Failed > 0 {
				log.Printf("Deposits of %s: %d blocks, %d detected, %d credited, %d held, %d failed",
					d.watcher.Network, res.Blocks, res.Detected, res.Credited, res.Held, res.Failed)
			}
		}
	}()
	return worker{"deposits of " + d.watcher.Network, func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}}
}
