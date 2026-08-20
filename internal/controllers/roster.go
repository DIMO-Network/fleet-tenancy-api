package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// maxVehicleMetadataTokens bounds one request. The whole roster is 619 rows
// today and the largest single fleet is a few hundred, so this is far above any
// real list — it is here so one HTTP request cannot become an unbounded query,
// not to constrain legitimate use. A caller that genuinely outgrows it should
// page rather than have the limit raised without thought.
const maxVehicleMetadataTokens = 5000

// RosterController serves the minted-vehicle roster — what a vehicle is and who
// owns it, reconciled from identity-api rather than authored here.
//
// It answers ONE question and deliberately not two: given token ids, what are
// these vehicles? Which vehicles a caller may see is answered elsewhere, by
// entitlements, memberships and groups, and combining the two here would put a
// second opinion about the set in the codebase — the duplication plan 07 exists
// to remove.
type RosterController struct {
	logger  *zerolog.Logger
	roster  *service.RosterService
	tenants *service.TenantService
	caller  CallerResolver
}

func NewRosterController(logger *zerolog.Logger, roster *service.RosterService,
	tenants *service.TenantService, caller CallerResolver) *RosterController {
	return &RosterController{logger: logger, roster: roster, tenants: tenants, caller: caller}
}

// VehicleMetadata — POST /v1/tenants/:tenantId/vehicle-metadata
//
// A POST because the input is a list whose length is the caller's fleet, not a
// query parameter — the same reasoning as shareable-owners, and the same
// hazard avoided: six hundred token ids in a query string is several kilobytes
// of request line, which fiber's read buffer will refuse long before anyone
// suspects the URL.
//
// Nothing is written.
//
// The tenant in the path authorizes the CALLER, in the ordinary way every route
// here does. It is not a per-vehicle filter, and the roster is not partitioned
// by tenant — a vehicle's owner and definition are properties of the vehicle,
// which is why step 3's table is keyed by token id alone. What stops this being
// a roster dump is that the caller must name the tokens: there is no listing,
// no wildcard and no paging cursor, so a caller learns nothing here it did not
// already know somewhere it was gated.
func (c *RosterController) VehicleMetadata(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.VehicleMetadataInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "read vehicle metadata"); err != nil {
		return err
	}
	if len(body.TokenIDs) > maxVehicleMetadataTokens {
		return fiber.NewError(fiber.StatusBadRequest, "too many token ids in one request")
	}

	// An empty list is 200 and an empty result, not 400. A tenant whose fleet
	// resolved to nothing is a real state — a customer between entitlements, a
	// member scoped to an empty group — and answering it with an error would
	// make the caller's normal path handle a failure that is not one.
	rows, err := c.roster.Metadata(ctx.Context(), dedupeTokenIDs(body.TokenIDs))
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Int("requested", len(body.TokenIDs)).
			Msg("read vehicle metadata")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read vehicle metadata")
	}
	return ctx.JSON(models.VehicleMetadataResult{Vehicles: rows})
}

// dedupeTokenIDs collapses repeats while keeping the caller's order, so a list
// built by concatenating two overlapping sets cannot produce the same vehicle
// twice in the response.
func dedupeTokenIDs(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (c *RosterController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
	if tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenant id is required")
	}
	caller := c.caller(ctx)
	ok, err := c.tenants.CallerMayAccess(ctx.Context(), caller, tenantID)
	if err != nil {
		if errors.Is(err, service.ErrInvalidTenantID) {
			return fiber.NewError(fiber.StatusBadRequest, "tenant id must be a uuid")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("caller scope check")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	if !ok {
		c.logger.Warn().
			Str("caller_tenant", callerID(caller)).
			Str("subject_tenant", tenantID).
			Str("op", op).
			Msg("/v1: caller read vehicle metadata for a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}
