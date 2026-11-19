// Package tronsigner is the signer of the TRON custody (#68): a service of
// its own, apart from the exchange's server, which holds the seed and signs
// the payments of the hot wallet. The server holds only the xpub of the
// deposit addresses; it asks the signer to pay a withdrawal, over mutual
// TLS, and the signer builds the transaction itself, checks it against its
// caps, signs, and broadcasts it.
package tronsigner

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/mdastpak/crypto-exchange/internal/tron"
)

// Config is the configuration of the signer, read from its YAML file.
type Config struct {
	// Listen is the address it serves on, such as ":8443".
	Listen string `yaml:"listen"`
	// NodeURL is the HTTP API of a TRON node; its key is TRONGRID_API_KEY.
	NodeURL string `yaml:"node_url"`
	TLS     struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
		// ClientCAFile is the CA that the server's client certificate is
		// signed by; AllowedClients the common names that may call.
		ClientCAFile   string   `yaml:"client_ca_file"`
		AllowedClients []string `yaml:"allowed_clients"`
	} `yaml:"tls"`
	// Seed is where the seed is: encrypted by a key of Vault's Transit
	// engine, which decrypts it at the start with VAULT_TOKEN. The seed
	// itself is never in a file.
	Seed struct {
		VaultAddr      string `yaml:"vault_addr"`
		TransitKey     string `yaml:"transit_key"`
		CiphertextFile string `yaml:"ciphertext_file"`
	} `yaml:"seed"`
	// StatePath is the file of the payments that it signed, by key, and
	// what each day used of the caps.
	StatePath string `yaml:"state_path"`
	// FeeLimitSun is the most TRX, in sun, that a token payment may burn.
	FeeLimitSun int64 `yaml:"fee_limit_sun"`
	// Confirmations is how many blocks make a payment final: 19.
	Confirmations int64   `yaml:"confirmations"`
	Assets        []Asset `yaml:"assets"`
	// Sweeps move the deposits from the deposit addresses to the hot
	// wallet, or to ColdAddress while the hot wallet holds its HotCap.
	Sweeps struct {
		// ColdAddress is the cold storage, which this seed does not hold.
		ColdAddress string `yaml:"cold_address"`
		// TopUpSun is the TRX, in sun, that a deposit address needs for a
		// token sweep's fee, which the hot wallet sends it first.
		TopUpSun int64 `yaml:"top_up_sun"`
		// ReserveSun is the TRX, in sun, that a TRX sweep leaves on the
		// address for the bandwidth of the transfer.
		ReserveSun int64 `yaml:"reserve_sun"`

		cold tron.Address
	} `yaml:"sweeps"`
}

// Asset is an asset that the hot wallet pays, with its caps in the
// smallest unit. Only these are paid.
type Asset struct {
	Symbol string `yaml:"symbol"`
	// Contract is the TRC20 contract; empty for TRX.
	Contract       string `yaml:"contract"`
	MaxPerTransfer string `yaml:"max_per_transfer"`
	MaxPerDay      string `yaml:"max_per_day"`
	// HotCap is the most of the asset, in the smallest unit, that the hot
	// wallet keeps: a sweep that would take it above goes to the cold
	// storage. Empty keeps every sweep in the hot wallet.
	HotCap string `yaml:"hot_cap"`

	token    tron.Address
	hotCap   *big.Int
	perOne   *big.Int
	perDay   *big.Int
	isNative bool
}

// LoadConfig reads and checks the configuration. A field that it does not
// know is an error.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path of the configuration, given by the operator.
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("tron-signer: %s: %w", path, err)
	}
	return &c, c.Check()
}

// Check checks the configuration, and readies its assets.
func (c *Config) Check() error {
	var problems []error
	need := func(v, name string) {
		if v == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	need(c.Listen, "listen")
	need(c.NodeURL, "node_url")
	need(c.TLS.CertFile, "tls.cert_file")
	need(c.TLS.KeyFile, "tls.key_file")
	need(c.TLS.ClientCAFile, "tls.client_ca_file")
	need(c.Seed.VaultAddr, "seed.vault_addr")
	need(c.Seed.TransitKey, "seed.transit_key")
	need(c.Seed.CiphertextFile, "seed.ciphertext_file")
	need(c.StatePath, "state_path")
	if len(c.TLS.AllowedClients) == 0 {
		problems = append(problems, errors.New("tls.allowed_clients names no client"))
	}
	if c.FeeLimitSun <= 0 {
		problems = append(problems, errors.New("fee_limit_sun must be positive"))
	}
	if c.Confirmations <= 0 {
		c.Confirmations = 19
	}
	if c.Sweeps.ColdAddress != "" {
		if a, err := tron.ParseAddress(c.Sweeps.ColdAddress); err != nil {
			problems = append(problems, fmt.Errorf("sweeps.cold_address: %w", err))
		} else {
			c.Sweeps.cold = a
		}
	}
	if c.Sweeps.TopUpSun <= 0 {
		c.Sweeps.TopUpSun = c.FeeLimitSun
	}
	if c.Sweeps.ReserveSun < 0 {
		problems = append(problems, errors.New("sweeps.reserve_sun must not be negative"))
	}
	if len(c.Assets) == 0 {
		problems = append(problems, errors.New("assets names no asset"))
	}
	seen := map[string]bool{}
	for i := range c.Assets {
		a := &c.Assets[i]
		if a.Symbol == "" || seen[a.Symbol] {
			problems = append(problems, fmt.Errorf("assets[%d]: a symbol, once", i))
			continue
		}
		seen[a.Symbol] = true
		if a.Contract == "" {
			a.isNative = true
		} else if t, err := tron.ParseAddress(a.Contract); err != nil {
			problems = append(problems, fmt.Errorf("assets[%d].contract: %w", i, err))
		} else {
			a.token = t
		}
		var ok1, ok2 bool
		a.perOne, ok1 = new(big.Int).SetString(a.MaxPerTransfer, 10)
		a.perDay, ok2 = new(big.Int).SetString(a.MaxPerDay, 10)
		if a.HotCap != "" {
			limit, ok := new(big.Int).SetString(a.HotCap, 10)
			if !ok || limit.Sign() <= 0 {
				problems = append(problems, fmt.Errorf("assets[%d].hot_cap is a positive integer in the smallest unit", i))
			} else if c.Sweeps.ColdAddress == "" {
				problems = append(problems, fmt.Errorf("assets[%d].hot_cap needs sweeps.cold_address", i))
			}
			a.hotCap = limit
		}
		if !ok1 || !ok2 || a.perOne.Sign() <= 0 || a.perDay.Cmp(a.perOne) < 0 {
			problems = append(problems, fmt.Errorf("assets[%d]: max_per_transfer and max_per_day are positive integers in the smallest unit, the day at least the transfer", i))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("tron-signer: %w", errors.Join(problems...))
	}
	return nil
}

// asset is the asset of a request: by its symbol, or a token by its
// contract, which the server names it by.
func (c *Config) asset(name string) (*Asset, bool) {
	for i := range c.Assets {
		if c.Assets[i].Symbol == name || (c.Assets[i].Contract != "" && c.Assets[i].Contract == name) {
			return &c.Assets[i], true
		}
	}
	return nil, false
}
