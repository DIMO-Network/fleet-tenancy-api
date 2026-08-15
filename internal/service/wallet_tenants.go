package service

import (
	"context"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/lib/pq"
)

// ErrInvalidSurface is returned for a surface outside the accepted set, so a
// caller's typo answers 400 rather than silently returning the unfiltered list.
var ErrInvalidSurface = fmt.Errorf("surface must be %q or %q", models.SurfaceFleetLite, models.SurfaceB2B)

// ListTenantsForWallet answers "which tenants does this wallet belong to" —
// the question fleet-lite's own tenant list asks at login, and the one call
// this service could not answer before: every other read here starts from a
// tenant id, and a person logging in does not have one yet.
//
// Direct memberships only, deliberately. A delegation is an operator's
// management right over a customer, and this listing exists to open sessions —
// surfacing delegated tenants here is how impersonation would sneak back in.
//
// The caller's scope bounds the answer exactly as CallerMayAccess bounds every
// other read: each row must be a tenant the caller could ask about
// individually — itself, a child holding no license of its own, or a tenant it
// holds a delegation over. A service caller sees every membership. The filter
// is the same expression CallerMayAccess runs, applied per row in SQL, so the
// list can never disclose a tenant whose detail read would 403.
func (s *TenantService) ListTenantsForWallet(ctx context.Context, caller *models.CallerTenant, wallet, surface string) ([]models.WalletTenant, error) {
	if caller == nil {
		return nil, fmt.Errorf("caller is required")
	}
	if wallet == "" {
		return nil, fmt.Errorf("wallet is required")
	}

	q := `SELECT t.id, t.name, t.kind, t.entitlement_mode,
	             m.role, m.permissions, m.scope_group_ids
	        FROM memberships m
	        JOIN tenants t ON t.id = m.tenant_id
	       WHERE lower(m.wallet) = lower($1)`
	args := []any{wallet}

	switch surface {
	case models.SurfaceFleetLite:
		// A suspended tenant would refuse the session at authz anyway;
		// filtering it here keeps the list honest rather than offering a door
		// that will not open. fleet_lite_enabled=false is the operator saying
		// "not a selectable fleet" — see the design set.
		q += ` AND t.status = 'active' AND t.fleet_lite_enabled`
	case models.SurfaceB2B:
		q += ` AND t.kind = 'operator'`
	case "":
		// Unfiltered.
	default:
		return nil, ErrInvalidSurface
	}

	if !caller.IsService {
		args = append(args, caller.TenantID)
		p := fmt.Sprintf("$%d", len(args))
		q += ` AND (t.id = ` + p + `::uuid
		        OR (t.parent_tenant_id = ` + p + `::uuid
		            AND NOT EXISTS (SELECT 1 FROM tenant_credentials c
		                             WHERE c.tenant_id = t.id
		                               AND c.dimo_client_id IS NOT NULL))
		        OR EXISTS (SELECT 1 FROM tenant_delegations d
		                    WHERE d.operator_tenant_id = ` + p + `::uuid
		                      AND d.customer_tenant_id = t.id))`
	}

	q += ` ORDER BY lower(t.name), t.id`

	rows, err := s.pdb.DBS().Reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tenants for wallet: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.WalletTenant{}
	for rows.Next() {
		var (
			wt          models.WalletTenant
			permsJSON   []byte
			scopeGroups pq.StringArray
		)
		if err := rows.Scan(&wt.TenantID, &wt.Name, &wt.Kind, &wt.EntitlementMode,
			&wt.Role, &permsJSON, &scopeGroups); err != nil {
			return nil, fmt.Errorf("scan wallet tenant: %w", err)
		}
		wt.Permissions = parsePermissions(permsJSON)
		// nil stays nil (unrestricted); an empty array stays empty (restricted
		// to nothing) — the same three-valued encoding as everywhere else.
		if scopeGroups != nil {
			wt.ScopeGroupIDs = []string(scopeGroups)
		}
		out = append(out, wt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants for wallet: %w", err)
	}
	return out, nil
}
