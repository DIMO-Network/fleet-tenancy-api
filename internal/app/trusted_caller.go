package app

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// TrustedCallerHeader carries the pre-shared key. Deliberately not
// Authorization: that header already carries the caller's developer-license
// JWT, and the two answer different questions.
const TrustedCallerHeader = "X-Tenancy-Key" //nolint:gosec // header name, not a credential

// TrustedCallerLocalsKey holds the name the presented key belongs to.
const TrustedCallerLocalsKey = "trustedCaller"

// NewTrustedCallerGuard gates /v1 on a pre-shared key held by each trusted
// application.
//
// WHAT THIS IS AND IS NOT. It answers "is this a trusted application?" and
// nothing else. It does not say which tenant the caller may act for — the
// developer-license JWT and TenantService.CallerMayAccess do that. Collapsing
// the two would make every key holder able to read every tenant, which is the
// property the scope rule exists to prevent.
//
// It runs before the JWT middleware on purpose: it is the cheapest check, and
// an untrusted caller should be turned away before the service does signature
// verification or touches the database on its behalf.
//
// The alternative considered was linkerd mesh identity, which needs no shared
// secret at all. It was rejected after an attempt at it took every service in
// the namespace out for about a minute: the namespace-wide Server has an empty
// podSelector, and attaching an HTTPRoute to it makes the proxy 404 every path
// that does not match a route. A mechanism whose blast radius is that hard to
// predict is the wrong place for this service's front door.
func NewTrustedCallerGuard(keys map[string]string, logger *zerolog.Logger) fiber.Handler {
	// Snapshot into a slice so the comparison loop is allocation-free and does
	// not depend on map iteration order.
	type namedKey struct{ name, key string }
	known := make([]namedKey, 0, len(keys))
	for n, k := range keys {
		known = append(known, namedKey{name: n, key: k})
	}

	return func(c *fiber.Ctx) error {
		presented := c.Get(TrustedCallerHeader)
		if presented == "" {
			logger.Debug().Str("path", c.Path()).Msg("/v1: no trusted-caller key presented")
			return fiber.NewError(fiber.StatusUnauthorized, "missing "+TrustedCallerHeader)
		}

		// Every candidate is compared, and in constant time, so neither the
		// number of comparisons nor their duration reveals which key was close.
		// The loop deliberately does not break on a match.
		matched := ""
		for _, k := range known {
			if subtle.ConstantTimeCompare([]byte(presented), []byte(k.key)) == 1 {
				matched = k.name
			}
		}
		if matched == "" {
			// The presented value is never logged: a near-miss in the logs is a
			// credential in the logs.
			logger.Warn().Str("path", c.Path()).Msg("/v1: unrecognised trusted-caller key")
			return fiber.NewError(fiber.StatusUnauthorized, "invalid "+TrustedCallerHeader)
		}

		c.Locals(TrustedCallerLocalsKey, matched)
		return c.Next()
	}
}

// TrustedCallerFrom returns the name of the application whose key was accepted,
// or "" if the guard did not run.
func TrustedCallerFrom(c *fiber.Ctx) string {
	name, _ := c.Locals(TrustedCallerLocalsKey).(string)
	return name
}
