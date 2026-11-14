package config

import (
	"slices"
	"strings"
)

// smsProblems reports unknown or repeated SMS providers, both provider and
// providers set, and failover bounds out of range (#371).
func (c *Configuration) smsProblems(p *problems) {
	s := c.Notifications.SMS
	if s.Provider != "" && len(s.Providers) > 0 {
		p.add("notifications.sms.provider", "is set beside notifications.sms.providers; set one of them")
	}
	seen := map[string]bool{}
	for _, name := range s.ProviderNames() {
		key := "notifications.sms.providers"
		if len(s.Providers) == 0 {
			key = "notifications.sms.provider"
		}
		switch {
		case !slices.Contains(SMSProviders, name):
			p.add(key, "has %q; the providers are %s", name, strings.Join(SMSProviders, " and "))
		case seen[name]:
			p.add(key, "has %q twice", name)
		}
		seen[name] = true
	}
	f := s.Failover
	if f.BreakerFailures < 0 || f.BreakerFailures > 100 {
		p.add("notifications.sms.failover.breaker_failures", "is %d; it must be from 1 to 100, or 0 for the default", f.BreakerFailures)
	}
	if f.BreakerSeconds < 0 || f.BreakerSeconds > 3600 {
		p.add("notifications.sms.failover.breaker_seconds", "is %d; it must be from 1 to 3600, or 0 for the default", f.BreakerSeconds)
	}
	if f.MinDeliveryPercent > 100 {
		p.add("notifications.sms.failover.min_delivery_percent", "is %d; it must be at most 100, or negative to turn it off", f.MinDeliveryPercent)
	}
	if f.MinSamples < 0 {
		p.add("notifications.sms.failover.min_samples", "is %d; it must be at least 1, or 0 for the default", f.MinSamples)
	}
}
