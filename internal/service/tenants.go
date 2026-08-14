package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/lib/pq"
)

var (
	// ErrNameTaken is returned when an operator already has a customer by that
	// name. Uniqueness is per operator, not global — two operators may each
	// have a customer called Acme.
	ErrNameTaken = errors.New("a customer with that name already exists")

	// ErrNotAnOperator is returned when a non-operator tenant tries to create a
	// customer beneath itself.
	ErrNotAnOperator = errors.New("only an operator tenant may have customers")

	// ErrInvalidStatus is returned for a status outside the accepted set.
	ErrInvalidStatus = errors.New("status must be active or suspended")
)

// ListChildren returns the customer tenants under an operator, with the counts
// the console list shows.
//
// Counts are computed per request rather than stored. A denormalised counter is
// a second copy of a fact, and the entitlement and membership rows are the
// first — these are two aggregates over small, indexed tables, and a count that
// disagrees with the list it summarises is worse than a slightly slower query.
func (s *TenantService) ListChildren(ctx context.Context, operatorID string) ([]models.Tenant, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT t.id, t.name, t.kind, t.parent_tenant_id, t.status, t.managed,
		        t.entitlement_mode, t.fleet_lite_enabled, t.memberships_enforced,
		        t.external_ref, t.created_at,
		        (SELECT count(*) FROM vehicle_entitlements e
		          WHERE e.tenant_id = t.id AND e.revoked_at IS NULL),
		        (SELECT count(*) FROM memberships m WHERE m.tenant_id = t.id),
		        (SELECT max(m.last_login_at) FROM memberships m WHERE m.tenant_id = t.id)
		   FROM tenants t
		  WHERE t.parent_tenant_id = $1
		  ORDER BY lower(t.name)`, operatorID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", operatorID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.Tenant{}
	for rows.Next() {
		t, err := scanTenantWithCounts(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list children of %s: %w", operatorID, err)
	}
	return out, nil
}

// Get returns one tenant with its counts.
func (s *TenantService) Get(ctx context.Context, tenantID string) (*models.Tenant, error) {
	row := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT t.id, t.name, t.kind, t.parent_tenant_id, t.status, t.managed,
		        t.entitlement_mode, t.fleet_lite_enabled, t.memberships_enforced,
		        t.external_ref, t.created_at,
		        (SELECT count(*) FROM vehicle_entitlements e
		          WHERE e.tenant_id = t.id AND e.revoked_at IS NULL),
		        (SELECT count(*) FROM memberships m WHERE m.tenant_id = t.id),
		        (SELECT max(m.last_login_at) FROM memberships m WHERE m.tenant_id = t.id)
		   FROM tenants t WHERE t.id = $1`, tenantID)

	t, err := scanTenantWithCounts(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// CreateCustomer creates a managed customer tenant under an operator, plus the
// delegation that lets the operator manage it.
//
// The delegation is written here rather than inferred from parent_tenant_id.
// Authorization always checks the delegation row, so revoking an operator's
// management rights is a single delete, and a future operator-of-operator
// arrangement needs no schema change.
//
// Both writes are one transaction: a customer whose operator cannot manage it
// is a tenant nobody can reach.
func (s *TenantService) CreateCustomer(ctx context.Context, operatorID string, in *models.CreateTenantInput, actorWallet string) (*models.Tenant, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	var kind string
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT kind FROM tenants WHERE id = $1`, operatorID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load operator %s: %w", operatorID, err)
	}
	if kind != models.KindOperator {
		return nil, ErrNotAnOperator
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO tenants (name, kind, parent_tenant_id, managed, entitlement_mode, external_ref)
		 VALUES ($1, $2, $3, TRUE, $4, $5)
		 RETURNING id`,
		name, models.KindCustomer, operatorID, models.EntitlementExplicit,
		nullableString(in.ExternalRef)).Scan(&id)
	if err != nil {
		// idx_tenants_name_per_parent — unique on (parent, lower(name)).
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, fmt.Errorf("insert tenant: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO tenant_delegations (operator_tenant_id, customer_tenant_id, scopes, created_by_wallet)
		 VALUES ($1, $2, $3, NULLIF($4, ''))`,
		operatorID, id, pq.StringArray(models.DelegationScopes), actorWallet); err != nil {
		return nil, fmt.Errorf("insert delegation: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().Str("operator_tenant", operatorID).Str("customer_tenant", id).
		Str("name", name).Str("actor", actorWallet).Msg("customer tenant created")

	return s.Get(ctx, id)
}

// Update patches a tenant. Absent fields are left alone.
func (s *TenantService) Update(ctx context.Context, tenantID string, in *models.UpdateTenantInput) (*models.Tenant, error) {
	sets := []string{}
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", clause, len(args)))
	}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, fmt.Errorf("name cannot be empty")
		}
		add("name", name)
	}
	if in.Status != nil {
		if *in.Status != models.StatusActive && *in.Status != models.StatusSuspended {
			return nil, ErrInvalidStatus
		}
		add("status", *in.Status)
	}
	if in.FleetLiteEnabled != nil {
		add("fleet_lite_enabled", *in.FleetLiteEnabled)
	}
	if in.MembershipsEnforced != nil {
		add("memberships_enforced", *in.MembershipsEnforced)
	}
	if in.ExternalRef != nil {
		// Distinct from absent: an explicit "" clears it.
		add("external_ref", nullableString(in.ExternalRef))
	}

	if len(sets) == 0 {
		return s.Get(ctx, tenantID)
	}

	args = append(args, tenantID)
	q := fmt.Sprintf(`UPDATE tenants SET %s, updated_at = NOW() WHERE id = $%d`,
		strings.Join(sets, ", "), len(args))

	res, err := s.pdb.DBS().Writer.ExecContext(ctx, q, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameTaken
		}
		return nil, fmt.Errorf("update tenant %s: %w", tenantID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return nil, ErrTenantNotFound
	}

	if in.Status != nil {
		// Worth its own line: suspension is the one change here that takes
		// access away from people, and it is eventually consistent by the authz
		// cache window rather than immediate.
		s.logger.Info().Str("tenant_id", tenantID).Str("status", *in.Status).
			Msg("tenant status changed")
	}
	return s.Get(ctx, tenantID)
}

// ListMembers returns the memberships of a tenant.
func (s *TenantService) ListMembers(ctx context.Context, tenantID string) ([]models.Member, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT m.wallet, u.email, m.role, m.permissions, m.scope_group_ids,
		        m.granted_by_tenant_id, m.granted_by_wallet, m.last_login_at, m.created_at
		   FROM memberships m
		   LEFT JOIN users u ON u.wallet = m.wallet
		  WHERE m.tenant_id = $1
		  ORDER BY lower(COALESCE(u.email, m.wallet))`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.Member{}
	for rows.Next() {
		var (
			m           models.Member
			email       sql.NullString
			permsJSON   []byte
			scopeGroups pq.StringArray
			grantedTen  sql.NullString
			grantedWal  sql.NullString
			lastLogin   sql.NullTime
			createdAt   sql.NullTime
		)
		if err := rows.Scan(&m.Wallet, &email, &m.Role, &permsJSON, &scopeGroups,
			&grantedTen, &grantedWal, &lastLogin, &createdAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.Email = nullStringPtr(email)
		m.Permissions = parsePermissions(permsJSON)
		// nil stays nil (unrestricted); an empty array stays empty (restricted
		// to nothing). Normalising one into the other here would invert the
		// meaning for every caller downstream.
		if scopeGroups != nil {
			m.ScopeGroupIDs = []string(scopeGroups)
		}
		m.GrantedByTenantID = nullStringPtr(grantedTen)
		m.GrantedByWallet = nullStringPtr(grantedWal)
		m.LastLoginAt = nullTimePtr(lastLogin)
		if createdAt.Valid {
			m.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members of %s: %w", tenantID, err)
	}
	return out, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTenantWithCounts(r rowScanner) (*models.Tenant, error) {
	var (
		t            models.Tenant
		parent       sql.NullString
		externalRef  sql.NullString
		createdAt    sql.NullTime
		lastActivity sql.NullTime
	)
	if err := r.Scan(&t.ID, &t.Name, &t.Kind, &parent, &t.Status, &t.Managed,
		&t.EntitlementMode, &t.FleetLiteEnabled, &t.MembershipsEnforced,
		&externalRef, &createdAt,
		&t.VehicleCount, &t.UserCount, &lastActivity); err != nil {
		return nil, err
	}
	t.ParentTenantID = nullStringPtr(parent)
	t.ExternalRef = nullStringPtr(externalRef)
	if createdAt.Valid {
		t.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	t.LastActivity = nullTimePtr(lastActivity)
	return &t, nil
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

func nullTimePtr(n sql.NullTime) *string {
	if !n.Valid {
		return nil
	}
	v := n.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	return &v
}

// nullableString turns an optional, possibly-empty string into a NULL-able
// argument, so "" clears the column rather than storing an empty string that
// every reader then has to treat as absent.
func nullableString(s *string) any {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return strings.TrimSpace(*s)
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}
