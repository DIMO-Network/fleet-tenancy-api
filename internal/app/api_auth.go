package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
)

// CallerLocalsKey holds the calling tenant resolved from the developer-license
// JWT. Handlers read it with CallerFrom.
const CallerLocalsKey = "caller"

// NewDeveloperLicenseTenantResolver returns Fiber middleware that identifies the
// *caller* of /v1 from their developer-license JWT's `ethereum_address` claim,
// matched against tenant_credentials.dimo_client_id.
//
// It assumes jwtware has already verified the signature against the DIMO JWKS
// upstream — this only maps a verified identity onto a tenant. Registering it
// without jwtware in front would authenticate nothing, since the claim would be
// attacker-controlled.
//
// Two differences from kaufmann-oracle's version, which this otherwise mirrors:
//
//   - It matches on lower(dimo_client_id), because that is exactly the
//     expression tenant_credentials' unique index is built on. Matching any
//     other way would either miss a row or use a different notion of identity
//     than the constraint that guarantees uniqueness.
//   - There is no qm.Limit(1). kaufmann needs one because its tenants table
//     permits duplicate client ids — a comment there says duplicates "shouldn't
//     happen, but the data model allows it", and in production two pairs did.
//     Here the unique index makes a second row impossible, so a duplicate would
//     be a schema violation and deserves to surface, not to be silently
//     truncated to whichever row sorted first.
func NewDeveloperLicenseTenantResolver(pdb *db.Store, logger *zerolog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userToken, ok := c.Locals("user").(*jwt.Token)
		if !ok || userToken == nil {
			return fiber.NewError(fiber.StatusUnauthorized, "missing JWT")
		}
		claims, ok := userToken.Claims.(jwt.MapClaims)
		if !ok {
			return fiber.NewError(fiber.StatusUnauthorized, "invalid JWT claims")
		}
		ethAddr, _ := claims["ethereum_address"].(string)
		if ethAddr == "" {
			return fiber.NewError(fiber.StatusUnauthorized, "ethereum_address claim is required")
		}

		caller, err := lookupCallerByClientID(c.Context(), pdb, ethAddr)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// Deliberately 403 and not 404: the license is a valid DIMO
				// identity, it just isn't registered here. Distinguishing the
				// two would let any license holder probe which ones we know.
				logger.Warn().Str("ethereum_address", strings.ToLower(ethAddr)).
					Msg("/v1: no tenant registered for this developer license")
				return fiber.NewError(fiber.StatusForbidden, "no tenant is registered for this developer license")
			}
			logger.Err(err).Msg("/v1: caller tenant lookup failed")
			return fiber.NewError(fiber.StatusInternalServerError, "tenant lookup failed")
		}

		c.Locals(CallerLocalsKey, caller)
		return c.Next()
	}
}

// lookupCallerByClientID resolves a developer-license client id to its tenant.
func lookupCallerByClientID(ctx context.Context, pdb *db.Store, clientID string) (*models.CallerTenant, error) {
	var caller models.CallerTenant
	err := pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT t.id, t.name, t.kind, t.status
		   FROM tenant_credentials tc
		   JOIN tenants t ON t.id = tc.tenant_id
		  WHERE lower(tc.dimo_client_id) = lower($1)`,
		clientID).Scan(&caller.TenantID, &caller.Name, &caller.Kind, &caller.Status)
	if err != nil {
		return nil, err
	}
	caller.ClientID = clientID
	return &caller, nil
}

// CallerFrom returns the caller resolved by the middleware, if any. Handlers
// that can run both with and without the resolver must tolerate a nil return
// rather than assume it is present.
func CallerFrom(c *fiber.Ctx) *models.CallerTenant {
	caller, _ := c.Locals(CallerLocalsKey).(*models.CallerTenant)
	return caller
}
