package app

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/kyc"
)

// kycDocumentPath is the route of the content of a KYC document, which
// its signed links name.
const kycDocumentPath = helpers.APIPrefix + "/admin/kyc/documents/%s/content"

// useKYC gives the user and admin routes the KYC applications (#72) while
// compliance.kyc.storage is set and the data keys are loaded, which seal
// the personal data. Without either, the routes answer kyc_unavailable.
func useKYC(h *InfraHandlers, db *sql.DB, settings *config.Configuration) error {
	cfg := settings.Compliance.KYC.Storage
	if cfg.Endpoint == "" {
		log.Printf("compliance.kyc.storage is not set: the KYC applications are off")
		return nil
	}
	if h.DataKeys == nil {
		log.Printf("WARNING: the KYC applications need the data keys, which seal the personal data: they are off")
		return nil
	}
	store, err := kyc.NewS3(kyc.S3Config{Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket,
		AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, UseTLS: cfg.UseTLS, ServerSideEncryption: cfg.ServerSideEncryption})
	if err != nil {
		return err
	}
	s := kyc.Service{DB: db, Store: store, LinkPath: kycDocumentPath}
	// A provider verifies the new applications (#72); the region check
	// keeps Sumsub to the international region.
	if p := settings.KYCProvider(); p != nil {
		s.Provider = &kyc.Sumsub{BaseURL: p.Endpoint, AppToken: p.APIKey, SecretKey: p.APISecret, WebhookSecret: p.WebhookSecret,
			Level: p.Level, Client: &http.Client{Timeout: time.Duration(max(p.Timeout, 10)) * time.Second}}
		log.Printf("KYC applications are verified by %s", s.Provider.Name())
	}
	h.User.KYC, h.Admin.KYC = s, s
	return nil
}
