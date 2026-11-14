package app

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/antiphishing"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/notify"
)

// antiPhishingStore is the store of the anti-phishing codes (#375), with
// the configured rotation.
func antiPhishingStore(conn *sql.DB) antiphishing.Store {
	return antiphishing.Store{DB: conn, Rotation: antiphishing.NewRotation(config.Config.AntiPhishing.Rotation)}
}

// startAntiPhishingReminders starts the job that reminds users to change
// an old or much-shown anti-phishing code (#375), every
// anti_phishing.rotation.check_interval_minutes. Every instance runs it:
// each claims the rows it reminds, so that a user is reminded once. It
// returns nil when no code is ever due for a change.
func startAntiPhishingReminders(conn *sql.DB, sender notify.OTPSender) *worker {
	rotation := config.Config.AntiPhishing.Rotation
	if !rotation.Enabled() || sender == nil {
		return nil
	}
	r := &antiPhishingReminder{
		reminders: antiphishing.Reminders{Store: antiPhishingStore(conn), Sender: sender,
			SMS: config.Config.Notifications.SMS.Kavenegar.AntiPhishingReminderTemplate != "" || config.Config.OTP.Auth.DevLogCodes},
		interval: time.Duration(rotation.CheckIntervalMinutes) * time.Minute,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	r.start()
	return &worker{"anti-phishing reminders", r.Stop}
}

// antiPhishingReminder runs the reminders at once and then every interval,
// until Stop.
type antiPhishingReminder struct {
	reminders antiphishing.Reminders
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
}

func (r *antiPhishingReminder) start() {
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

func (r *antiPhishingReminder) pass() {
	ctx, cancel := context.WithTimeout(context.Background(), r.interval)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	n, err := r.reminders.Run(ctx)
	if err != nil && ctx.Err() == nil {
		log.Printf("Failed to send the reminders to change the anti-phishing codes: %v", err)
	}
	if n > 0 {
		log.Printf("Reminded %d users to change their anti-phishing codes", n)
	}
}

// Stop ends the job and waits for a run in progress, until ctx ends.
func (r *antiPhishingReminder) Stop(ctx context.Context) error {
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
