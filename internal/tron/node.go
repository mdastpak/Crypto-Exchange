package tron

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Block is a block, with the transfers of TRX and TRC20 tokens that it
// holds.
type Block struct {
	Number    int64
	ID        string
	ParentID  string
	Transfers []Transfer
}

// Transfer is a transfer of a successful transaction.
type Transfer struct {
	TxID string
	// Index is the index of the log of a TRC20 transfer in its
	// transaction, and -1 for a transfer of TRX.
	Index int
	// Token is the contract of a TRC20 token; the zero Address for TRX.
	Token    Address
	From, To Address
	// Amount is in the smallest unit: sun for TRX, the token's for a token.
	Amount *big.Int
}

// Node reads the chain: TronGrid, or a full node with the same HTTP API.
type Node struct {
	BaseURL string
	// APIKey is TronGrid's key, sent as TRON-PRO-API-KEY.
	APIKey string
	Client *http.Client
}

// transferEvent is the topic of the TRC20 Transfer(address,address,uint256)
// event.
const transferEvent = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

func (n Node) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.APIKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", n.APIKey)
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("tron: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("tron: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tron: %s: status %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("tron: %s: %w", path, err)
	}
	return nil
}

type blockJSON struct {
	BlockID     string `json:"blockID"`
	BlockHeader struct {
		RawData struct {
			Number     int64  `json:"number"`
			ParentHash string `json:"parentHash"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Transactions []struct {
		TxID string `json:"txID"`
		Ret  []struct {
			ContractRet string `json:"contractRet"`
		} `json:"ret"`
		RawData struct {
			Contract []struct {
				Type      string `json:"type"`
				Parameter struct {
					Value struct {
						Amount       int64  `json:"amount"`
						OwnerAddress string `json:"owner_address"`
						ToAddress    string `json:"to_address"`
					} `json:"value"`
				} `json:"parameter"`
			} `json:"contract"`
		} `json:"raw_data"`
	} `json:"transactions"`
}

// Head returns the newest block, without its transfers.
func (n Node) Head(ctx context.Context) (Block, error) {
	var b blockJSON
	if err := n.post(ctx, "/wallet/getnowblock", struct{}{}, &b); err != nil {
		return Block{}, err
	}
	if b.BlockID == "" {
		return Block{}, fmt.Errorf("tron: getnowblock answered no block")
	}
	return Block{Number: b.BlockHeader.RawData.Number, ID: b.BlockID, ParentID: b.BlockHeader.RawData.ParentHash}, nil
}

type infoJSON struct {
	ID          string `json:"id"`
	BlockNumber int64  `json:"blockNumber"`
	// Result is FAILED for a transaction that failed.
	Result  string `json:"result"`
	Receipt struct {
		Result string `json:"result"`
	} `json:"receipt"`
	Log []struct {
		Address string   `json:"address"`
		Topics  []string `json:"topics"`
		Data    string   `json:"data"`
	} `json:"log"`
}

// Block returns block number, with its TRX transfers and the TRC20
// Transfer events of its successful transactions.
func (n Node) Block(ctx context.Context, number int64) (Block, error) {
	var b blockJSON
	if err := n.post(ctx, "/wallet/getblockbynum", map[string]int64{"num": number}, &b); err != nil {
		return Block{}, err
	}
	if b.BlockID == "" || b.BlockHeader.RawData.Number != number {
		return Block{}, fmt.Errorf("tron: block %d not found", number)
	}
	out := Block{Number: number, ID: b.BlockID, ParentID: b.BlockHeader.RawData.ParentHash}
	for _, tx := range b.Transactions {
		if len(tx.Ret) == 0 || tx.Ret[0].ContractRet != "SUCCESS" || len(tx.RawData.Contract) != 1 {
			continue
		}
		c := tx.RawData.Contract[0]
		if c.Type != "TransferContract" || c.Parameter.Value.Amount <= 0 {
			continue
		}
		from, err1 := ParseAddress(c.Parameter.Value.OwnerAddress)
		to, err2 := ParseAddress(c.Parameter.Value.ToAddress)
		if err1 != nil || err2 != nil {
			continue
		}
		out.Transfers = append(out.Transfers, Transfer{TxID: tx.TxID, Index: -1, From: from, To: to,
			Amount: big.NewInt(c.Parameter.Value.Amount)})
	}
	var infos []infoJSON
	if err := n.post(ctx, "/wallet/gettransactioninfobyblocknum", map[string]int64{"num": number}, &infos); err != nil {
		return Block{}, err
	}
	for _, info := range infos {
		if info.Receipt.Result != "SUCCESS" {
			continue
		}
		for i, l := range info.Log {
			t, ok := tokenTransfer(info.ID, i, l.Address, l.Topics, l.Data)
			if ok {
				out.Transfers = append(out.Transfers, t)
			}
		}
	}
	return out, nil
}

// tokenTransfer reads a TRC20 Transfer event: the contract and the topics
// are 20-byte and 32-byte hex without the prefix of an address.
func tokenTransfer(txID string, index int, contract string, topics []string, data string) (Transfer, bool) {
	if len(topics) != 3 || !strings.EqualFold(topics[0], transferEvent) || len(topics[1]) != 64 || len(topics[2]) != 64 {
		return Transfer{}, false
	}
	token, err1 := ParseAddress("41" + contract)
	from, err2 := ParseAddress("41" + topics[1][24:])
	to, err3 := ParseAddress("41" + topics[2][24:])
	raw, err4 := hex.DecodeString(data)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || len(raw) != 32 {
		return Transfer{}, false
	}
	amount := new(big.Int).SetBytes(raw)
	if amount.Sign() <= 0 {
		return Transfer{}, false
	}
	return Transfer{TxID: txID, Index: index, Token: token, From: from, To: to, Amount: amount}, true
}

// TxBlock returns the block of a transaction in the canonical chain; false
// for one that is not in it, as after a reorg.
func (n Node) TxBlock(ctx context.Context, txID string) (int64, bool, error) {
	var info infoJSON
	if err := n.post(ctx, "/wallet/gettransactioninfobyid", map[string]string{"value": txID}, &info); err != nil {
		return 0, false, err
	}
	if info.ID == "" || info.BlockNumber == 0 {
		return 0, false, nil
	}
	return info.BlockNumber, true, nil
}

// Receipt is what the chain did with a transaction.
type Receipt struct {
	// Found is false for a transaction in no block yet, or never taken.
	Found bool
	Block int64
	// Success is false for a transaction that failed in its block, such as
	// a token transfer that reverted or ran out of energy: it moved
	// nothing, though it may have burned its fee.
	Success bool
	// Result is the chain's word for a failure, such as REVERT.
	Result string
}

// Receipt returns what the chain did with transaction id.
func (n Node) Receipt(ctx context.Context, id string) (Receipt, error) {
	var info infoJSON
	if err := n.post(ctx, "/wallet/gettransactioninfobyid", map[string]string{"value": id}, &info); err != nil {
		return Receipt{}, err
	}
	if info.ID == "" || info.BlockNumber == 0 {
		return Receipt{}, nil
	}
	r := Receipt{Found: true, Block: info.BlockNumber, Success: true}
	switch {
	case info.Result == "FAILED":
		r.Success, r.Result = false, "FAILED"
	case info.Receipt.Result != "" && info.Receipt.Result != "SUCCESS":
		r.Success, r.Result = false, info.Receipt.Result
	}
	return r, nil
}

// ErrBroadcast is a transaction that the node refused.
var ErrBroadcast = errors.New("tron: the node refused the transaction")

// Broadcast sends a signed transaction to the network. One that the node
// already has is sent.
func (n Node) Broadcast(ctx context.Context, t Tx) error {
	var out struct {
		Result  bool   `json:"result"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := n.post(ctx, "/wallet/broadcasthex", map[string]string{"transaction": hex.EncodeToString(t.Encode())}, &out); err != nil {
		return err
	}
	if out.Result || out.Code == "DUP_TRANSACTION_ERROR" {
		return nil
	}
	msg := out.Message
	if raw, err := hex.DecodeString(msg); err == nil {
		msg = string(raw)
	}
	return fmt.Errorf("%w: %s %s", ErrBroadcast, out.Code, msg)
}
