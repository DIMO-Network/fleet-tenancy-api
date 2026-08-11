package service

import (
	"context"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/google/uuid"
)

// ErrInvalidTenantID is returned for a subject that is not a uuid at all, so a
// caller's typo surfaces as 400 rather than as a database error turned 500.
var ErrInvalidTenantID = fmt.Errorf("tenant id is not a valid uuid")

// CallerMayAccess reports whether a caller may ask /v1 about a subject tenant.
//
// The rule mirrors the architecture's credential resolution rule — "a tenant's
// effective credential is its own if it has one, otherwise its parent's" — so
// there is one notion of whose license reaches which tenant rather than two that
// can disagree. A caller may ask about:
//
//  1. itself;
//  2. a child whose effective credential is the caller's, meaning the child is
//     parented to the caller and holds no license of its own;
//  3. a tenant it holds a delegation over, which is how an operator reaches a
//     customer that does have its own credential.
//
// Service callers bypass it entirely. That flag exists because the rule above
// deliberately cannot express a shared proxy acting across tenants.
//
// Note what this is NOT: a check that the caller equals the subject. That would
// pass today, while every tenant is unparented, and break on the first
// operator-managed customer — which holds no license and is reached with its
// operator's. Case 2 is the whole reason this is a query rather than a string
// comparison.
func (s *TenantService) CallerMayAccess(ctx context.Context, caller *models.CallerTenant, subjectTenantID string) (bool, error) {
	if caller == nil {
		return false, nil
	}
	if caller.IsService {
		return true, nil
	}
	if _, err := uuid.Parse(subjectTenantID); err != nil {
		return false, ErrInvalidTenantID
	}
	if caller.TenantID == subjectTenantID {
		return true, nil
	}

	var allowed bool
	err := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT
		  EXISTS (SELECT 1
		            FROM tenants t
		            LEFT JOIN tenant_credentials c ON c.tenant_id = t.id
		           WHERE t.id = $2::uuid
		             AND t.parent_tenant_id = $1::uuid
		             AND c.dimo_client_id IS NULL)
		  OR
		  EXISTS (SELECT 1
		            FROM tenant_delegations d
		           WHERE d.operator_tenant_id = $1::uuid
		             AND d.customer_tenant_id = $2::uuid)`,
		caller.TenantID, subjectTenantID).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("caller scope check: %w", err)
	}
	return allowed, nil
}
