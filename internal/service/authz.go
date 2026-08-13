package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// DefaultAuthzCacheTTLSeconds is how long callers may reuse an authz answer.
//
// This endpoint is on the hot path of two apps, so callers cache. The tradeoff
// is that membership revocation is eventually consistent by up to this window —
// documented rather than hidden, because "I removed them and they could still
// get in" is otherwise a support call.
const DefaultAuthzCacheTTLSeconds = 60

// AuthzService answers "what may this wallet do in this tenant?".
type AuthzService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewAuthzService(logger *zerolog.Logger, pdb *db.Store) *AuthzService {
	return &AuthzService{logger: logger, pdb: pdb}
}

// Authorize resolves a wallet's access to a tenant.
//
// Two ways in, checked in that order:
//
//  1. Direct membership — a memberships row.
//  2. Delegation — the wallet is a member of an operator tenant that holds a
//     tenant_delegations row over this tenant. Management only; it never grants a
//     fleet session. fleet-lite must refuse a delegated answer.
//
// Authorization always checks the delegation row rather than parent_tenant_id
// directly, so revoking is a single delete and a future operator-of-operator
// arrangement needs no schema change.
func (s *AuthzService) Authorize(ctx context.Context, tenantID, wallet string) (*models.AuthzResult, error) {
	if tenantID == "" || wallet == "" {
		return nil, fmt.Errorf("tenantID and wallet are required")
	}
	// Wallets are stored EIP-55 checksummed; normalise so a lowercase caller
	// still matches. fleet-lite lowercases, kaufmann checksums — this service
	// must not care which one is asking.
	checksummed := common.HexToAddress(wallet).Hex()

	result := &models.AuthzResult{
		TenantID:        tenantID,
		Wallet:          checksummed,
		Via:             models.ViaNone,
		Permissions:     []string{},
		CacheTTLSeconds: DefaultAuthzCacheTTLSeconds,
	}

	var status string
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil // unknown tenant is "no access", not an error
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}
	result.TenantStatus = status

	// A suspended tenant grants nobody anything, membership or delegation.
	//
	// Enforced here rather than left to callers. The field has been on the
	// response since the first release and *no caller has ever read it* —
	// kaufmann and fleet-lite both decode tenantStatus and neither checks it —
	// so suspending a tenant was decorative: the operator console would say the
	// customer's users can no longer sign in, and they still could. Answering
	// it in the one place that answers the question makes it true everywhere,
	// with no caller change.
	//
	// TenantStatus is still returned so a caller can say *why* rather than
	// showing a bare denial.
	if status != models.StatusActive {
		return result, nil
	}

	// 1. Direct membership.
	var (
		role        string
		permsJSON   []byte
		scopeGroups pq.StringArray
	)
	err = s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT role, permissions, scope_group_ids
		   FROM memberships WHERE tenant_id = $1 AND lower(wallet) = lower($2)`,
		tenantID, checksummed).Scan(&role, &permsJSON, &scopeGroups)
	if err == nil {
		result.Member = true
		result.Role = role
		result.Via = models.ViaDirect
		result.Permissions = parsePermissions(permsJSON)
		if scopeGroups != nil {
			result.ScopeGroupIDs = []string(scopeGroups)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load membership: %w", err)
	}

	// 2. Delegation: any operator tenant this wallet belongs to that holds a
	// delegation over the target. Permissions come from the delegation scopes,
	// not from a membership row in the target tenant — the wallet has none.
	var (
		operatorTenantID string
		scopes           pq.StringArray
	)
	err = s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT d.operator_tenant_id, d.scopes
		   FROM tenant_delegations d
		   JOIN memberships m ON m.tenant_id = d.operator_tenant_id
		  WHERE d.customer_tenant_id = $1 AND lower(m.wallet) = lower($2)
		  LIMIT 1`,
		tenantID, checksummed).Scan(&operatorTenantID, &scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load delegation: %w", err)
	}

	result.Member = false // delegated access is not membership
	result.Via = models.ViaDelegation
	result.OperatorTenantID = operatorTenantID
	result.Permissions = []string(scopes)
	// A delegate is never group-restricted; scope belongs to memberships.
	return result, nil
}

// parsePermissions reads the permissions JSONB array, tolerating null/empty.
func parsePermissions(raw []byte) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "[]" {
		return []string{}
	}
	var out []string
	if err := jsonUnmarshalStrings(raw, &out); err != nil {
		return []string{}
	}
	return out
}
