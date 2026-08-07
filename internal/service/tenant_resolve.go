package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/rs/zerolog"
)

// ErrTenantNotFound is returned when no tenant matches a lookup.
var ErrTenantNotFound = errors.New("tenant not found")

// TenantService answers tenant lookups for the service-to-service surface.
type TenantService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewTenantService(logger *zerolog.Logger, pdb *db.Store) *TenantService {
	return &TenantService{logger: logger, pdb: pdb}
}

// ResolveByClientID maps a DIMO developer-license client id to its tenant. This
// replaces kaufmann-oracle's resolver, which is the reason it exists: once an
// operator's license is shared with its customers, "which tenant is this
// license" stops being answerable inside any one app.
//
// Matched on lower(dimo_client_id) — the exact expression the unique index is
// built on, so the lookup and the uniqueness guarantee agree. A client id with
// no tenant is ErrTenantNotFound rather than an empty result, because the
// distinction matters to the caller: "unknown license" and "known license,
// nothing configured" would otherwise be the same answer.
func (s *TenantService) ResolveByClientID(ctx context.Context, clientID string) (*models.TenantRef, error) {
	if clientID == "" {
		return nil, fmt.Errorf("clientID is required")
	}

	var (
		ref    models.TenantRef
		parent sql.NullString
	)
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT t.id, t.name, t.kind, t.status, t.parent_tenant_id,
		        t.entitlement_mode, t.fleet_lite_enabled
		   FROM tenant_credentials tc
		   JOIN tenants t ON t.id = tc.tenant_id
		  WHERE lower(tc.dimo_client_id) = lower($1)`,
		clientID).Scan(&ref.TenantID, &ref.Name, &ref.Kind, &ref.Status, &parent,
		&ref.EntitlementMode, &ref.FleetLiteEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve client id: %w", err)
	}
	if parent.Valid {
		ref.ParentTenantID = &parent.String
	}
	return &ref, nil
}
