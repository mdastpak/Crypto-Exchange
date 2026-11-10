package main

import (
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/handlers/admin"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
)

// adminRoutes is the one table of the admin routes that change something
// (#308), by method and route pattern under /api/v1/admin. A sensitive route
// needs a step-up: it changes what one stolen admin token must not be enough
// for, such as locking the platform, freezing a user, scheduling a delisting
// or a migration, overriding a capability, or re-sealing a row. A route that
// changes something and is not listed here is refused, and a test fails.
var adminRoutes = map[string]admin.RouteClass{
	// The administrator's own sign-in.
	"POST /step-up":        admin.RouteChange,
	"POST /logout":         admin.RouteChange,
	"DELETE /sessions/:id": admin.RouteChange,

	// The currency list.
	"POST /currencies":                       admin.RouteChange,
	"PUT /currencies/:hk":                    admin.RouteChange,
	"DELETE /currencies/:hk":                 admin.RouteChange,
	"POST /currencies/:hk/restore":           admin.RouteChange,
	"DELETE /currencies/:hk/symbols/:symbol": admin.RouteChange,

	// What users can do with a currency, and when it leaves.
	"POST /currencies/:hk/migrations":                 admin.RouteSensitive,
	"POST /currencies/:hk/migrations/:id/cancel":      admin.RouteSensitive,
	"POST /currencies/:hk/delistings":                 admin.RouteSensitive,
	"POST /currencies/:hk/delistings/:id/cancel":      admin.RouteSensitive,
	"PUT /currencies/:hk/capabilities/:capability":    admin.RouteSensitive,
	"DELETE /currencies/:hk/capabilities/:capability": admin.RouteSensitive,

	// Freezing users and locking the platform.
	"POST /users/:hk/restrictions":          admin.RouteSensitive,
	"POST /users/:hk/restrictions/:id/lift": admin.RouteSensitive,
	"POST /platform/controls":               admin.RouteSensitive,
	"PATCH /platform/controls/:id":          admin.RouteSensitive,
	"POST /platform/controls/:id/lift":      admin.RouteSensitive,

	// Accepting a row that was changed outside the backend.
	"POST /integrity/findings/:id/reseal": admin.RouteSensitive,

	// The market data catalog: public prices of other exchanges, which the
	// jobs can fetch again.
	"POST /market-data/sources/:source/pause":  admin.RouteChange,
	"POST /market-data/sources/:source/resume": admin.RouteChange,
	"POST /market-data/venues/:id/enable":      admin.RouteChange,
	"POST /market-data/venues/:id/disable":     admin.RouteChange,
	"POST /market-data/assets/:id/hide":        admin.RouteChange,
	"POST /market-data/assets/:id/unhide":      admin.RouteChange,
	"POST /market-data/assets/:id/link":        admin.RouteChange,
	"POST /market-data/assets/:id/unlink":      admin.RouteChange,
	"PUT /market-data/assets/:id/tier":         admin.RouteChange,
	"PUT /market-data/pairs/:id/tier":          admin.RouteChange,
	"PUT /market-data/pairs/:id/mapping":       admin.RouteChange,
	"POST /market-data/conflicts/:id/resolve":  admin.RouteChange,
	"POST /market-data/jobs/:job/run":          admin.RouteChange,
}

// adminRoutePrefix is where the admin routes are registered.
const adminRoutePrefix = helpers.APIPrefix + "/admin"

// adminRouteClass returns the class of an admin route by its method and
// full pattern. A route that only reads needs no entry.
func adminRouteClass(method, route string) admin.RouteClass {
	if class, ok := adminRoutes[method+" "+strings.TrimPrefix(route, adminRoutePrefix)]; ok {
		return class
	}
	if method == "GET" || method == "HEAD" {
		return admin.RouteRead
	}
	return admin.RouteUnclassified
}
