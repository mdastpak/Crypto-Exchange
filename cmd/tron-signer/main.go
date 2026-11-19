// Command tron-signer is the signer of the TRON custody (#68), which holds
// the seed and signs the payments of the hot wallet. It runs apart from the
// exchange's server, which calls it over mutual TLS. See
// docs/custody.md#the-signer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/redirect"

	"github.com/mdastpak/crypto-exchange/internal/tron"
	"github.com/mdastpak/crypto-exchange/internal/tronsigner"
)

func main() {
	path := flag.String("config", "config/tron-signer.yaml", "the configuration file")
	xpub := flag.Bool("xpub", false, "print the xpub of the deposit addresses and the hot wallet's address, and exit")
	flag.Parse()
	if err := run(*path, *xpub); err != nil {
		log.Fatal(err)
	}
}

func run(path string, xpub bool) error {
	cfg, err := tronsigner.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	seed, err := tronsigner.UnwrapSeed(ctx, &http.Client{Timeout: 20 * time.Second, CheckRedirect: redirect.Refuse}, cfg, os.Getenv("VAULT_TOKEN"))
	if err != nil {
		return err
	}
	node := tron.Node{BaseURL: cfg.NodeURL, APIKey: os.Getenv("TRONGRID_API_KEY"), Client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: redirect.Refuse}}
	signer, err := tronsigner.New(cfg, node, seed, nil)
	clear(seed)
	if err != nil {
		return err
	}
	if xpub {
		fmt.Printf("xpub (custody.providers[].xpub): %s\nhot wallet (custody.providers[].hot_wallet_address): %s\n",
			signer.DepositXPub(), signer.HotWallet())
		return nil
	}
	tlsConfig, err := tronsigner.ServerTLS(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: tronsigner.Handler(signer, cfg.TLS.AllowedClients), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	log.Printf("tron-signer: the hot wallet %s pays on %q", signer.HotWallet(), cfg.Listen) //nolint:gosec // G706: the operator's own address, quoted.
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServeTLS("", "") }()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
