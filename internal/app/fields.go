package app

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// fieldsRekeyInterval is how often an instance runs db.RekeyFields. A run
// reads every row of user_contacts and users, as the seal verifier does,
// and writes only those that need it.
const fieldsRekeyInterval = time.Hour

// useFieldEncryption encrypts the personal fields with the data keys
// (#140), and starts the job that encrypts the rows written before, or
// with an older key. Without data keys, which only development allows, the
// fields stay in plaintext.
func useFieldEncryption(conn *sql.DB, dataKeys *keys.Live) worker {
	field.Use(field.New(dataKeys))
	r := &fieldsRekeyer{conn: conn, interval: fieldsRekeyInterval, stop: make(chan struct{}), done: make(chan struct{})}
	r.start()
	return worker{"field rekeyer", r.Stop}
}

// fieldsRekeyer runs db.RekeyFields at once and then every interval, until
// Stop. Instances run it at the same time without harm: each skips the rows
// that another holds.
type fieldsRekeyer struct {
	conn     *sql.DB
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

func (r *fieldsRekeyer) start() {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			r.pass()
			select {
			case <-ticker.C:
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *fieldsRekeyer) pass() {
	ctx, cancel := context.WithTimeout(context.Background(), r.interval)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	report, err := db.RekeyFields(ctx, r.conn)
	switch {
	case err != nil && ctx.Err() == nil:
		log.Printf("Failed to encrypt the personal fields: %v", err)
	case report.Contacts > 0 || report.Users > 0 || report.Skipped > 0:
		log.Printf("Encrypted the personal fields of %d contacts and %d users with the current keys; %d rows skipped",
			report.Contacts, report.Users, report.Skipped)
	}
}

// Stop ends the job and waits for a run in progress, until ctx ends.
func (r *fieldsRekeyer) Stop(ctx context.Context) error {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
