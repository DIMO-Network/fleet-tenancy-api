package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

var (
	// ErrEntitlementNotFound is returned when revoking something not entitled.
	ErrEntitlementNotFound = errors.New("entitlement not found")

	// ErrNotExplicitMode is returned for a tenant whose fleet is not defined by
	// these rows. An operator or self-serve tenant resolves its fleet from its
	// licence's privileged set, so an entitlement written for it would be a row
	// nothing reads — refused rather than silently accepted.
	ErrNotExplicitMode = errors.New("this tenant's fleet is not defined by entitlements")
)

// EntitlementService decides which vehicles a customer tenant may see.
//
// THIS IS THE ISOLATION BOUNDARY. Under D2/D5 the tenant boundary is not
// enforced by the chain: every customer's vehicle data is reachable with the
// operator's developer JWT, and which customer sees which vehicle is these
// rows. That makes this the mitigation rather than defence in depth, which is
// why the invariant below is enforced twice — here with a good error message,
// and again by a unique index that a race cannot slip past.
type EntitlementService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewEntitlementService(logger *zerolog.Logger, pdb *db.Store) *EntitlementService {
	return &EntitlementService{logger: logger, pdb: pdb}
}

// List returns a tenant's active entitlements.
//
// Token ids and provenance only. VIN, plate and model are not here and should
// not be: they belong to the oracle, and copying them in would make this
// service a fleet-data store with a second, staler copy of the truth. The
// console joins against the oracle's vehicle list to render a row.
func (s *EntitlementService) List(ctx context.Context, tenantID string) ([]models.Entitlement, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT vehicle_token_id, source, source_group_id, granted_by_wallet, created_at
		   FROM vehicle_entitlements
		  WHERE tenant_id = $1 AND revoked_at IS NULL
		  ORDER BY vehicle_token_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list entitlements of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.Entitlement{}
	for rows.Next() {
		var (
			e         models.Entitlement
			group     sql.NullString
			grantedBy sql.NullString
			createdAt sql.NullTime
		)
		if err := rows.Scan(&e.VehicleTokenID, &e.Source, &group, &grantedBy, &createdAt); err != nil {
			return nil, fmt.Errorf("scan entitlement: %w", err)
		}
		e.SourceGroupID = nullStringPtr(group)
		e.GrantedByWallet = nullStringPtr(grantedBy)
		if createdAt.Valid {
			e.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list entitlements of %s: %w", tenantID, err)
	}
	return out, nil
}

// Assign entitles vehicles to a tenant.
//
// PARTIAL SUCCESS IS A NORMAL OUTCOME, not an error. An operator selecting
// forty vehicles, two of which another customer already holds, wants the
// thirty-eight and a clear account of the two — not a failed request and no way
// to tell which two. So conflicts come back per vehicle with the holder's name.
//
// Re-assigning a vehicle the tenant already holds is a no-op rather than a
// conflict: the caller asked for a state and it already obtains.
func (s *EntitlementService) Assign(ctx context.Context, tenantID string, in *models.AssignVehiclesInput, actorWallet string) (*models.AssignResult, error) {
	if len(in.TokenIDs) == 0 {
		return &models.AssignResult{Assigned: []int64{}, Rejected: []models.RejectedVehicle{}}, nil
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The tenant must exist and must be one whose fleet is defined by these
	// rows at all. An implicit-mode tenant resolves its fleet from its
	// licence's privileged set, so writing entitlements for it would create
	// rows nothing reads — a silent no-op is worse than a refusal.
	var (
		mode   string
		parent sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT entitlement_mode, parent_tenant_id FROM tenants WHERE id = $1`,
		tenantID).Scan(&mode, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}
	if mode != models.EntitlementExplicit {
		return nil, ErrNotExplicitMode
	}

	// Who else already holds any of these, among the explicit-mode tenants of
	// the same operator. This is the invariant as specified; the unique index
	// backing it is stricter and catches the race this read cannot.
	held, err := s.conflictingHolders(ctx, tx, tenantID, parent, in.TokenIDs)
	if err != nil {
		return nil, err
	}

	result := &models.AssignResult{Assigned: []int64{}, Rejected: []models.RejectedVehicle{}}
	for _, tokenID := range in.TokenIDs {
		if holder, taken := held[tokenID]; taken {
			result.Rejected = append(result.Rejected, models.RejectedVehicle{
				TokenID: tokenID,
				Reason:  "already entitled to another customer",
				HeldBy:  holder,
			})
			continue
		}

		// Each insert gets a savepoint.
		//
		// Not defensive clutter: in Postgres a constraint violation aborts the
		// whole transaction, so without this, one vehicle losing the race would
		// make every later statement fail with "current transaction is aborted"
		// and take the entire batch down with it. The savepoint is what makes
		// per-vehicle rejection actually possible rather than merely intended.
		if _, err := tx.ExecContext(ctx, `SAVEPOINT assign_one`); err != nil {
			return nil, fmt.Errorf("savepoint: %w", err)
		}

		// ON CONFLICT covers two cases at once: re-assigning a vehicle this
		// tenant already holds (idempotent), and re-assigning one it used to
		// hold, where clearing revoked_at brings the row back into force rather
		// than leaving a second row behind.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO vehicle_entitlements
			   (tenant_id, vehicle_token_id, source, source_group_id, granted_by_tenant_id, granted_by_wallet)
			 VALUES ($1, $2, 'operator', NULLIF($3, ''), $4, NULLIF($5, ''))
			 ON CONFLICT (tenant_id, vehicle_token_id) DO UPDATE SET
			   source_group_id   = EXCLUDED.source_group_id,
			   granted_by_wallet = COALESCE(EXCLUDED.granted_by_wallet, vehicle_entitlements.granted_by_wallet),
			   revoked_at        = NULL,
			   created_at        = NOW()`,
			tenantID, tokenID, in.FromGroupID, nullableParent(parent),
			normaliseOptionalWallet(actorWallet))
		if err != nil {
			if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT assign_one`); rbErr != nil {
				return nil, fmt.Errorf("rollback savepoint: %w", rbErr)
			}
			// The unique index fired, so somebody else took this vehicle
			// between the read above and this write. Reported the same way as a
			// conflict found by the read — from the operator's point of view it
			// is the same situation, only the holder's name is unknown because
			// it belongs to a transaction that had not committed when we looked.
			if isUniqueViolation(err) {
				result.Rejected = append(result.Rejected, models.RejectedVehicle{
					TokenID: tokenID,
					Reason:  "already entitled to another customer",
					HeldBy:  "another customer",
				})
				continue
			}
			return nil, fmt.Errorf("assign vehicle %d: %w", tokenID, err)
		}
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT assign_one`); err != nil {
			return nil, fmt.Errorf("release savepoint: %w", err)
		}
		result.Assigned = append(result.Assigned, tokenID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Int("assigned", len(result.Assigned)).
		Int("rejected", len(result.Rejected)).
		Str("from_group", in.FromGroupID).
		Str("actor", actorWallet).
		Msg("vehicles assigned")
	return result, nil
}

// Revoke ends an entitlement.
//
// Soft, by setting revoked_at. Knowing a vehicle *used to* belong to a customer
// matters for support and for cleaning up the rows fleet-lite cached for them,
// and a hard delete throws that away. The partial unique index ignores revoked
// rows, so the vehicle is immediately assignable elsewhere.
func (s *EntitlementService) Revoke(ctx context.Context, tenantID string, tokenID int64) error {
	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE vehicle_entitlements SET revoked_at = NOW()
		  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND revoked_at IS NULL`,
		tenantID, tokenID)
	if err != nil {
		return fmt.Errorf("revoke vehicle %d: %w", tokenID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrEntitlementNotFound
	}
	s.logger.Info().Str("tenant_id", tenantID).Int64("vehicle_token_id", tokenID).
		Msg("entitlement revoked")
	return nil
}

// conflictingHolders maps token id -> the name of the tenant already holding it.
//
// Scoped to the explicit-mode tenants under the same operator, which is the
// invariant as designed. Note what is deliberately NOT a conflict: the operator
// itself sees every vehicle including assigned ones, because it resolves its
// fleet implicitly from its licence and holds no rows here at all.
func (s *EntitlementService) conflictingHolders(ctx context.Context, tx *sql.Tx, tenantID string, parent sql.NullString, tokenIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}

	// An unparented tenant has no sibling customers to conflict with; the
	// unique index still guards against anything unexpected.
	if !parent.Valid {
		return out, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT e.vehicle_token_id, t.name
		   FROM vehicle_entitlements e
		   JOIN tenants t ON t.id = e.tenant_id
		  WHERE e.vehicle_token_id = ANY($1)
		    AND e.revoked_at IS NULL
		    AND e.tenant_id <> $2
		    AND t.parent_tenant_id = $3
		    AND t.entitlement_mode = $4`,
		pq.Array(tokenIDs), tenantID, parent.String, models.EntitlementExplicit)
	if err != nil {
		return nil, fmt.Errorf("check exclusivity: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var (
			tokenID int64
			name    string
		)
		if err := rows.Scan(&tokenID, &name); err != nil {
			return nil, fmt.Errorf("scan holder: %w", err)
		}
		out[tokenID] = name
	}
	return out, rows.Err()
}

// AssertEntitled is the choke point every path that turns a token id into a
// DIMO call is meant to go through.
//
// It exists here, unused by this service's own handlers, because the design
// makes it load-bearing: with isolation enforced by code rather than by the
// chain, one controller that forgets the check leaks one customer's telemetry
// to another. Callers get one function to call rather than each writing the
// query.
func (s *EntitlementService) AssertEntitled(ctx context.Context, tenantID string, tokenID int64) (bool, error) {
	var ok bool
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT true FROM vehicle_entitlements
		  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND revoked_at IS NULL`,
		tenantID, tokenID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("assert entitled: %w", err)
	}
	return ok, nil
}

// nullableParent keeps granted_by_tenant_id NULL when there is no operator,
// rather than storing an empty string the foreign key would reject.
func nullableParent(parent sql.NullString) any {
	if !parent.Valid {
		return nil
	}
	return parent.String
}
