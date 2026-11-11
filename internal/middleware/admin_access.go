package middleware

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
)

// CodeAdminNetworkDenied answers an admin route called from an address that
// admin.allowed_cidrs does not list.
const CodeAdminNetworkDenied = "admin_network_denied"

type adminListenerKey struct{}

// OnAdminListener marks the requests of the admin listener: the base context
// of the HTTP server on server.admin_listen_addr.
func OnAdminListener(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminListenerKey{}, true)
}

// FromAdminListener reports whether a request arrived on the admin listener.
func FromAdminListener(r *http.Request) bool {
	marked, _ := r.Context().Value(adminListenerKey{}).(bool)
	return marked
}

// AdminAccess limits where the admin routes can be reached from (#308), so
// that the public address of the API does not expose them.
type AdminAccess struct {
	// ListenerOnly serves the admin routes only on the admin listener. On
	// the public one they answer 404, as routes that do not exist.
	ListenerOnly bool
	// Allowed are the networks whose clients may call the admin routes.
	// Empty allows every address.
	Allowed []netip.Prefix
}

// ParseAdminAccess reads admin.allowed_cidrs, whose entries are CIDR ranges
// or single addresses.
func ParseAdminAccess(listenerOnly bool, cidrs []string) (AdminAccess, error) {
	access := AdminAccess{ListenerOnly: listenerOnly}
	for _, entry := range cidrs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, addrErr := netip.ParseAddr(entry)
			if addrErr != nil {
				return AdminAccess{}, fmt.Errorf("%q is neither a CIDR range nor an IP address", entry)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		access.Allowed = append(access.Allowed, prefix.Masked())
	}
	return access, nil
}

// Restricted reports whether the admin routes are limited at all.
func (a AdminAccess) Restricted() bool { return a.ListenerOnly || len(a.Allowed) > 0 }

// allows reports whether the client address is in an allowed network. An
// IPv4 address mapped into IPv6 counts as the IPv4 address.
func (a AdminAccess) allows(clientIP string) bool {
	if len(a.Allowed) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range a.Allowed {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Middleware refuses the requests that the access does not allow, before
// their token is read. The client address is the one that
// server.trusted_proxies resolves.
func (a AdminAccess) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.ListenerOnly && !FromAdminListener(c.Request) {
			helpers.SendResponse(c, http.StatusNotFound, i18n.M("NOT_FOUND"), nil)
			c.Abort()
			return
		}
		if !a.allows(c.ClientIP()) {
			log.Printf("Refused %s %s from %s: the address is not in admin.allowed_cidrs", c.Request.Method, c.FullPath(), c.ClientIP())
			helpers.SendError(c, http.StatusForbidden, CodeAdminNetworkDenied, i18n.M("ADMIN_NETWORK_DENIED"), nil)
			c.Abort()
			return
		}
		c.Next()
	}
}
