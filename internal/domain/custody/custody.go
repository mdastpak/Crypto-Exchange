package custody

import (
	"context"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// CustodyProvider defines the interface for asset custody operations. Every
// call takes a context.Context, whose deadline and cancellation bound the
// calls to nodes and providers; request-scoped values such as the request
// ID travel in it as well.
type CustodyProvider interface {
	// Provider info
	GetProviderType() CustodyProviderType
	GetSupportedAssets() []shared.UUID
	GetSupportedNetworks() []string

	// Deposit operations
	GenerateDepositAddress(ctx context.Context, userID, subAccountID shared.UUID, currencyHK shared.UUID) (*DepositAddress, error)
	GetDepositAddress(ctx context.Context, userID, subAccountID shared.UUID, currencyHK shared.UUID) (*DepositAddress, error)
	ValidateDepositAddress(address string, currencyHK shared.UUID) error

	// Deposit monitoring
	StartDepositMonitoring(ctx context.Context, currencyHK shared.UUID) error
	StopDepositMonitoring(ctx context.Context, currencyHK shared.UUID) error
	GetDepositStatus(ctx context.Context, txHash string) (*DepositStatus, error)

	// Withdrawal operations
	CreateWithdrawal(ctx context.Context, req *WithdrawalRequest) (*WithdrawalResult, error)
	ValidateWithdrawalAddress(address string, currencyHK shared.UUID) error
	EstimateWithdrawalFee(ctx context.Context, currencyHK shared.UUID, amount shared.Decimal, address string) (*FeeEstimate, error)
	GetWithdrawalStatus(ctx context.Context, withdrawalID shared.UUID) (*WithdrawalStatus, error)

	// Balance
	GetOnChainBalance(ctx context.Context, address string, currencyHK shared.UUID) (shared.Decimal, error)
	GetAddressesForUser(ctx context.Context, userID shared.UUID, currencyHK shared.UUID) ([]string, error)

	// Health
	HealthCheck(ctx context.Context) error
}

// CustodyProviderType identifies the custody implementation
type CustodyProviderType string

const (
	CustodyProviderInternal CustodyProviderType = "INTERNAL" // Off-chain ledger
	CustodyProviderBTC      CustodyProviderType = "BTC"      // Bitcoin UTXO
	CustodyProviderEVM      CustodyProviderType = "EVM"      // Ethereum, BSC, Polygon, Tron, etc.
	CustodyProviderSolana   CustodyProviderType = "SOLANA"   // Solana
	CustodyProviderFiat     CustodyProviderType = "FIAT"     // Bank integration
)

func (c CustodyProviderType) String() string {
	return string(c)
}

// DepositAddress represents a user's deposit address
type DepositAddress struct {
	ID           shared.UUID  `json:"id"`
	UserID       shared.UUID  `json:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty"`
	CurrencyHK   shared.UUID  `json:"currency_hk"`

	Address string `json:"address"`       // Main address
	Tag     string `json:"tag,omitempty"` // Memo/tag/payment_id

	// For HD wallets
	DerivationPath string `json:"derivation_path,omitempty"`
	Index          uint32 `json:"index,omitempty"`

	// Status
	Active bool `json:"active"`

	CreatedAt shared.Timestamp `json:"created_at"`
}

// DepositStatus represents the status of a deposit
type DepositStatus struct {
	TxHash     string      `json:"tx_hash"`
	CurrencyHK shared.UUID `json:"currency_hk"`
	Address    string      `json:"address"`
	Tag        string      `json:"tag,omitempty"`

	Amount          shared.Decimal `json:"amount"`
	ConfirmedAmount shared.Decimal `json:"confirmed_amount"` // After confirmations

	Confirmations    int `json:"confirmations"`
	RequiredConfirms int `json:"required_confirmations"`

	Status DepositStatusType `json:"status"`

	DetectedAt  shared.Timestamp  `json:"detected_at"`
	ConfirmedAt *shared.Timestamp `json:"confirmed_at,omitempty"`
	CreditedAt  *shared.Timestamp `json:"credited_at,omitempty"`

	// Raw blockchain data
	BlockHeight uint64 `json:"block_height"`
	BlockHash   string `json:"block_hash,omitempty"`
	RawData     string `json:"raw_data,omitempty"` // JSON
}

type DepositStatusType string

const (
	DepositStatusDetected   DepositStatusType = "DETECTED"   // Seen in mempool/block
	DepositStatusConfirming DepositStatusType = "CONFIRMING" // Waiting for confirmations
	DepositStatusConfirmed  DepositStatusType = "CONFIRMED"  // Confirmations reached
	DepositStatusCredited   DepositStatusType = "CREDITED"   // Credited to user balance
	DepositStatusFailed     DepositStatusType = "FAILED"     // Failed (reorg, double spend)
)

func (d DepositStatusType) String() string {
	return string(d)
}

// WithdrawalRequest represents a withdrawal request
type WithdrawalRequest struct {
	ID           shared.UUID  `json:"id"`
	UserID       shared.UUID  `json:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty"`
	CurrencyHK   shared.UUID  `json:"currency_hk"`

	Amount  shared.Decimal `json:"amount"`
	Address string         `json:"address"`
	Tag     string         `json:"tag,omitempty"`

	// Fee options
	FeeLevel  FeeLevel        `json:"fee_level,omitempty"`  // "slow", "normal", "fast", "custom"
	CustomFee *shared.Decimal `json:"custom_fee,omitempty"` // Custom fee amount

	// Options
	SubtractFee bool `json:"subtract_fee"` // Deduct fee from amount
	Priority    bool `json:"priority"`     // High priority

	// Idempotency
	IdempotencyKey string `json:"idempotency_key"`

	CreatedAt shared.Timestamp `json:"created_at"`
}

type FeeLevel string

const (
	FeeLevelSlow   FeeLevel = "SLOW"
	FeeLevelNormal FeeLevel = "NORMAL"
	FeeLevelFast   FeeLevel = "FAST"
	FeeLevelCustom FeeLevel = "CUSTOM"
)

func (f FeeLevel) String() string {
	return string(f)
}

// WithdrawalResult represents the result of a withdrawal creation
type WithdrawalResult struct {
	WithdrawalID shared.UUID          `json:"withdrawal_id"`
	TxHash       *string              `json:"tx_hash,omitempty"` // If broadcast immediately
	Status       WithdrawalStatusType `json:"status"`
	EstimatedFee shared.Decimal       `json:"estimated_fee"`
	NetAmount    shared.Decimal       `json:"net_amount"` // Amount - fee

	CreatedAt shared.Timestamp `json:"created_at"`
}

type WithdrawalStatusType string

const (
	WithdrawalStatusPending    WithdrawalStatusType = "PENDING"    // Created, not broadcast
	WithdrawalStatusBroadcast  WithdrawalStatusType = "BROADCAST"  // Sent to network
	WithdrawalStatusConfirming WithdrawalStatusType = "CONFIRMING" // Waiting for confirmations
	WithdrawalStatusCompleted  WithdrawalStatusType = "COMPLETED"  // Confirmed
	WithdrawalStatusFailed     WithdrawalStatusType = "FAILED"     // Failed
	WithdrawalStatusCancelled  WithdrawalStatusType = "CANCELLED"  // Cancelled
	WithdrawalStatusRejected   WithdrawalStatusType = "REJECTED"   // Rejected by compliance
)

func (w WithdrawalStatusType) String() string {
	return string(w)
}

// WithdrawalStatus represents withdrawal status
type WithdrawalStatus struct {
	WithdrawalID shared.UUID          `json:"withdrawal_id"`
	TxHash       string               `json:"tx_hash"`
	Status       WithdrawalStatusType `json:"status"`

	Amount    shared.Decimal `json:"amount"`
	Fee       shared.Decimal `json:"fee"`
	NetAmount shared.Decimal `json:"net_amount"`

	Confirmations    int `json:"confirmations"`
	RequiredConfirms int `json:"required_confirmations"`

	BroadcastAt *shared.Timestamp `json:"broadcast_at,omitempty"`
	ConfirmedAt *shared.Timestamp `json:"confirmed_at,omitempty"`

	ErrorMessage string `json:"error_message,omitempty"`
	RawData      string `json:"raw_data,omitempty"`
}

// FeeEstimate represents a fee estimate
type FeeEstimate struct {
	CurrencyHK    shared.UUID    `json:"currency_hk"`
	FeeLevel      FeeLevel       `json:"fee_level"`
	EstimatedFee  shared.Decimal `json:"estimated_fee"`
	EstimatedTime string         `json:"estimated_time"` // e.g., "10-30 minutes"
	MinFee        shared.Decimal `json:"min_fee"`
	MaxFee        shared.Decimal `json:"max_fee"`

	// For EVM
	GasPrice *shared.Decimal `json:"gas_price,omitempty"` // Gwei
	GasLimit *uint64         `json:"gas_limit,omitempty"`

	// For BTC
	SatPerVByte *uint64 `json:"sat_per_vbyte,omitempty"`

	UpdatedAt shared.Timestamp `json:"updated_at"`
}

// CustodyConfig is how a custody provider reaches one network of a
// currency: a row of custody_configs. The contract, the decimals, the
// limits, and the currency of the fee belong to the network of the
// currency, currency.Network, which it refers to (#309).
type CustodyConfig struct {
	ProviderType CustodyProviderType `json:"provider_type"`
	CurrencyHK   shared.UUID         `json:"currency_hk"`
	Network      string              `json:"network"`

	// Connection
	RPCEndpoints []string `json:"rpc_endpoints"`
	WSEndpoints  []string `json:"ws_endpoints,omitempty"`
	ExplorerAPI  string   `json:"explorer_api"`
	ExplorerWS   string   `json:"explorer_ws,omitempty"`

	// Authentication
	APIKey    string `json:"api_key,omitempty"`
	APISecret string `json:"api_secret,omitempty"`
	JWTToken  string `json:"jwt_token,omitempty"`

	// Chain-specific
	ChainID string `json:"chain_id,omitempty"` // For EVM

	// Deposit
	ConfirmationsRequired int `json:"confirmations_required"`

	// Withdrawal
	DefaultFeeLevel FeeLevel `json:"default_fee_level"`

	// Hot/Cold wallet
	HotWalletAddress  string `json:"hot_wallet_address"`
	ColdWalletAddress string `json:"cold_wallet_address,omitempty"`

	// Monitoring
	BlockPollInterval int `json:"block_poll_interval_seconds"` // For chains without WS
	ReorgDepth        int `json:"reorg_depth"`                 // Blocks to wait for finality

	// Compliance
	SanctionsScreening bool `json:"sanctions_screening"`

	Metadata map[string]string `json:"metadata,omitempty"`
}

// SettlementInstruction represents a settlement between parties
type SettlementInstruction struct {
	ID   shared.UUID    `json:"id"`
	Type SettlementType `json:"type"`

	// Parties
	FromUserID     shared.UUID  `json:"from_user_id"`
	ToUserID       shared.UUID  `json:"to_user_id"`
	FromSubAccount *shared.UUID `json:"from_sub_account,omitempty"`
	ToSubAccount   *shared.UUID `json:"to_sub_account,omitempty"`

	// Asset & amount
	CurrencyHK shared.UUID    `json:"currency_hk"`
	Amount     shared.Decimal `json:"amount"`

	// Settlement mode
	Mode shared.SettlementMode `json:"mode"`

	// For on-chain
	FromAddress string `json:"from_address,omitempty"`
	ToAddress   string `json:"to_address,omitempty"`
	TxHash      string `json:"tx_hash,omitempty"`

	// Reference
	ReferenceID   *shared.UUID `json:"reference_id,omitempty"`   // TradeID, OrderID
	ReferenceType string       `json:"reference_type,omitempty"` // "TRADE", "WITHDRAWAL", "DEPOSIT", "TRANSFER"

	// Status
	Status       SettlementStatus `json:"status"`
	ErrorMessage string           `json:"error_message,omitempty"`

	CreatedAt   shared.Timestamp  `json:"created_at"`
	ProcessedAt *shared.Timestamp `json:"processed_at,omitempty"`
	CompletedAt *shared.Timestamp `json:"completed_at,omitempty"`
}

type SettlementType string

const (
	SettlementTypeTrade       SettlementType = "TRADE"
	SettlementTypeWithdrawal  SettlementType = "WITHDRAWAL"
	SettlementTypeDeposit     SettlementType = "DEPOSIT"
	SettlementTypeTransfer    SettlementType = "TRANSFER"
	SettlementTypeFee         SettlementType = "FEE"
	SettlementTypeFunding     SettlementType = "FUNDING" // For derivatives
	SettlementTypeLiquidation SettlementType = "LIQUIDATION"
)

func (s SettlementType) String() string {
	return string(s)
}

type SettlementStatus string

const (
	SettlementStatusPending    SettlementStatus = "PENDING"
	SettlementStatusProcessing SettlementStatus = "PROCESSING"
	SettlementStatusCompleted  SettlementStatus = "COMPLETED"
	SettlementStatusFailed     SettlementStatus = "FAILED"
	SettlementStatusCancelled  SettlementStatus = "CANCELLED"
)

func (s SettlementStatus) String() string {
	return string(s)
}

// ChainConfig holds chain-specific configuration
type ChainConfig struct {
	Network string `json:"network"`
	ChainID string `json:"chain_id"`

	// Native asset
	NativeAsset    string `json:"native_asset"` // e.g., "BTC", "ETH", "TRX"
	NativeDecimals int    `json:"native_decimals"`

	// Contract address (for tokens)
	ContractAddress string `json:"contract_address,omitempty"`

	// RPC
	RPCEndpoints []string `json:"rpc_endpoints"`
	WSEndpoints  []string `json:"ws_endpoints,omitempty"`

	// Explorer
	ExplorerAPI string `json:"explorer_api"`
	ExplorerWS  string `json:"explorer_ws,omitempty"`

	// Block info
	BlockTime      int `json:"block_time_seconds"` // Average block time
	FinalityBlocks int `json:"finality_blocks"`    // Blocks for finality

	// Gas/Fee
	FeeAsset        string `json:"fee_asset"` // Asset to pay fees
	SupportsEIP1559 bool   `json:"supports_eip1559"`

	// Features
	SupportsTokens bool   `json:"supports_tokens"`
	TokenStandard  string `json:"token_standard,omitempty"` // "ERC20", "TRC20", "SPL"

	// Address format
	AddressPrefix string `json:"address_prefix,omitempty"` // e.g., "0x", "T", "1"
	AddressLength int    `json:"address_length"`
	RequiresTag   bool   `json:"requires_tag"`
	TagName       string `json:"tag_name,omitempty"` // "memo", "tag", "payment_id"

	// Custody
	HDPathTemplate string `json:"hd_path_template,omitempty"` // m/44'/coin'/account'/change/index

	Metadata map[string]string `json:"metadata,omitempty"`
}

// Predefined chain configs
var (
	BitcoinMainnet = ChainConfig{
		Network:        "bitcoin",
		ChainID:        "btc-mainnet",
		NativeAsset:    "BTC",
		NativeDecimals: 8,
		BlockTime:      600,
		FinalityBlocks: 6,
		FeeAsset:       "BTC",
		SupportsTokens: false,
		AddressPrefix:  "",
		AddressLength:  34,
		RequiresTag:    false,
	}

	BitcoinTestnet = ChainConfig{
		Network:        "bitcoin-testnet",
		ChainID:        "btc-testnet",
		NativeAsset:    "TBTC",
		NativeDecimals: 8,
		BlockTime:      600,
		FinalityBlocks: 6,
		FeeAsset:       "TBTC",
		SupportsTokens: false,
		AddressPrefix:  "tb1",
		AddressLength:  42,
		RequiresTag:    false,
	}

	TronMainnet = ChainConfig{
		Network:         "tron",
		ChainID:         "tron-mainnet",
		NativeAsset:     "TRX",
		NativeDecimals:  6,
		BlockTime:       3,
		FinalityBlocks:  19, // 19 blocks = ~1 minute
		FeeAsset:        "TRX",
		SupportsTokens:  true,
		TokenStandard:   "TRC20",
		SupportsEIP1559: false,
		AddressPrefix:   "T",
		AddressLength:   34,
		RequiresTag:     false,
	}

	TronTestnet = ChainConfig{
		Network:        "tron-shasta",
		ChainID:        "tron-shasta",
		NativeAsset:    "TRX",
		NativeDecimals: 6,
		BlockTime:      3,
		FinalityBlocks: 19,
		FeeAsset:       "TRX",
		SupportsTokens: true,
		TokenStandard:  "TRC20",
		AddressPrefix:  "T",
		AddressLength:  34,
		RequiresTag:    false,
	}

	EthereumMainnet = ChainConfig{
		Network:         "ethereum",
		ChainID:         "1",
		NativeAsset:     "ETH",
		NativeDecimals:  18,
		BlockTime:       12,
		FinalityBlocks:  64, // two epochs of 32 slots, about 13 minutes
		FeeAsset:        "ETH",
		SupportsTokens:  true,
		TokenStandard:   "ERC20",
		SupportsEIP1559: true,
		AddressPrefix:   "0x",
		AddressLength:   42,
		RequiresTag:     false,
	}

	// USDT on Tron (TRC20)
	USDT_TRC20 = ChainConfig{
		Network:         "tron",
		ChainID:         "tron-mainnet",
		NativeAsset:     "TRX",
		NativeDecimals:  6,
		BlockTime:       3,
		FinalityBlocks:  19,
		FeeAsset:        "TRX",
		SupportsTokens:  true,
		TokenStandard:   "TRC20",
		ContractAddress: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", // USDT TRC20
		AddressPrefix:   "T",
		AddressLength:   34,
		RequiresTag:     false,
	}
)
