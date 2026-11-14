package app

import (
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/smsrouting"
)

// useSMSRouting shares the health of the SMS providers between the
// instances through Redis, reads the routing that administrators choose
// from the database, and starts the delivery reports (#371). It returns the
// worker of the reports, or nil without SMS.
func useSMSRouting(router *notify.SMSRouter, services *models.InfraServices, handlers *InfraHandlers) *worker {
	if router == nil {
		return nil
	}
	f := config.Config.Notifications.SMS.Failover
	if services.Redis != nil && services.Redis.Client != nil {
		router.UseHealth(notify.NewRedisSMSHealth(services.Redis.Client,
			notify.SMSBreaker{Failures: f.BreakerFailures, Open: time.Duration(f.BreakerSeconds) * time.Second}))
	}
	store := smsrouting.Store{DB: services.Postgres.DB}
	router.UseRouting(store)
	handlers.Admin.SMS, handlers.Admin.SMSRoutes = router, store
	router.Start()
	return &worker{"SMS delivery reports", router.Stop}
}
