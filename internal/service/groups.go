package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

var (
	// ErrGroupNotFound is returned when no group matches (tenant, id).
	ErrGroupNotFound = errors.New("fleet group not found")

	// ErrGroupNameTaken covers both uniqueness rules at once: the exact-name
	// unique on (tenant_id, name) and an id collision, where two names slug to
	// the same value ("Vans" and "Vans!"). To a caller they are the same fact —
	// that name is not available in this tenant.
	ErrGroupNameTaken = errors.New("a group with that name already exists")

	// ErrInvalidGroupInput names the field-level mistakes: empty name, a name
	// that slugs to nothing, a colour that is not #RRGGBB.
	ErrInvalidGroupInput = errors.New("invalid group input")
)

var (
	slugNonAlphanum = regexp.MustCompile(`[^a-z0-9]+`)
	hexColor        = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// slugify matches fleet-lite's slug() byte for byte — ids minted here must be
// indistinguishable from ids minted there, or P3's backfill would produce two
// ids for one group.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlphanum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// GroupIDFor builds the R1-convention id: <tenant-uuid>_<slug>. '_' is
// unambiguous — a slug never contains one and a uuid never contains one — and
// the tenant prefix is what makes the id self-attributing in published
// attestations, where every producer under a shared operator license carries
// the same source.
func GroupIDFor(tenantID, name string) string {
	s := slugify(name)
	if s == "" {
		return ""
	}
	return tenantID + "_" + s
}

// GroupService owns fleet groups and their vehicle memberships: the record the
// two apps previously kept near-identical copies of, with this service as the
// single writer. Pure data access — publishing an outward attestation is P4's
// concern and deliberately not wired here.
type GroupService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewGroupService(logger *zerolog.Logger, pdb *db.Store) *GroupService {
	return &GroupService{logger: logger, pdb: pdb}
}

// List returns the tenant's groups with member counts, ordered by name.
func (s *GroupService) List(ctx context.Context, tenantID string) ([]models.FleetGroup, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT fg.id, fg.tenant_id, fg.name, fg.color,
		       (SELECT count(*) FROM vehicle_fleet_groups v
		         WHERE v.fleet_group_id = fg.id AND v.tenant_id = fg.tenant_id),
		       fg.created_at, fg.updated_at
		  FROM fleet_groups fg
		 WHERE fg.tenant_id = $1
		 ORDER BY fg.name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list groups of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.FleetGroup{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list groups of %s: %w", tenantID, err)
	}
	return out, nil
}

// ListWithVehicles returns the tenant's groups with their full member sets in
// one query. This is the whole-tenant read both apps' vehicle screens need and
// the one the P3 groups-diff compares — served in one round trip so a caller
// never assembles it from N per-group requests against a moving table.
func (s *GroupService) ListWithVehicles(ctx context.Context, tenantID string) ([]models.FleetGroupVehicles, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT fg.id, fg.tenant_id, fg.name, fg.color,
		       COALESCE(array_agg(v.vehicle_token_id ORDER BY v.vehicle_token_id)
		                FILTER (WHERE v.vehicle_token_id IS NOT NULL), '{}'),
		       fg.created_at, fg.updated_at
		  FROM fleet_groups fg
		  LEFT JOIN vehicle_fleet_groups v
		    ON v.fleet_group_id = fg.id AND v.tenant_id = fg.tenant_id
		 WHERE fg.tenant_id = $1
		 GROUP BY fg.id, fg.tenant_id, fg.name, fg.color, fg.created_at, fg.updated_at
		 ORDER BY fg.name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list vehicle groups of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.FleetGroupVehicles{}
	for rows.Next() {
		var (
			g                    models.FleetGroupVehicles
			tokenIDs             pq.Int64Array
			createdAt, updatedAt sql.NullTime
		)
		if err := rows.Scan(&g.ID, &g.TenantID, &g.Name, &g.Color, &tokenIDs,
			&createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan vehicle group: %w", err)
		}
		g.TokenIDs = []int64(tokenIDs)
		g.VehicleCount = len(g.TokenIDs)
		if createdAt.Valid {
			g.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if updatedAt.Valid {
			g.UpdatedAt = updatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list vehicle groups of %s: %w", tenantID, err)
	}
	return out, nil
}

// Get returns one group of the tenant.
func (s *GroupService) Get(ctx context.Context, tenantID, groupID string) (*models.FleetGroup, error) {
	row := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT fg.id, fg.tenant_id, fg.name, fg.color,
		       (SELECT count(*) FROM vehicle_fleet_groups v
		         WHERE v.fleet_group_id = fg.id AND v.tenant_id = fg.tenant_id),
		       fg.created_at, fg.updated_at
		  FROM fleet_groups fg
		 WHERE fg.tenant_id = $1 AND fg.id = $2`, tenantID, groupID)
	g, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	return g, err
}

// Create mints the id from the name and inserts the group.
func (s *GroupService) Create(ctx context.Context, tenantID string, in *models.CreateGroupInput) (*models.FleetGroup, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidGroupInput)
	}
	if !hexColor.MatchString(in.Color) {
		return nil, fmt.Errorf("%w: color must be #RRGGBB", ErrInvalidGroupInput)
	}
	id := GroupIDFor(tenantID, name)
	if id == "" {
		return nil, fmt.Errorf("%w: name yields an empty id", ErrInvalidGroupInput)
	}

	_, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`INSERT INTO fleet_groups (id, tenant_id, name, color) VALUES ($1, $2, $3, $4)`,
		id, tenantID, name, in.Color)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrGroupNameTaken
		}
		if isForeignKeyViolation(err) {
			return nil, ErrTenantNotFound
		}
		return nil, fmt.Errorf("create group %s: %w", id, err)
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("group_id", id).Msg("fleet group created")
	return s.Get(ctx, tenantID, id)
}

// Update renames or recolours. The id never changes on rename: scope_group_ids,
// source_group_id and every published attestation hold it, and an id that
// tracked the name would turn a rename into a cross-system migration.
func (s *GroupService) Update(ctx context.Context, tenantID, groupID string, in *models.UpdateGroupInput) (*models.FleetGroup, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{tenantID, groupID}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		args = append(args, strings.TrimSpace(*in.Name))
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if in.Color != nil && *in.Color != "" {
		if !hexColor.MatchString(*in.Color) {
			return nil, fmt.Errorf("%w: color must be #RRGGBB", ErrInvalidGroupInput)
		}
		args = append(args, *in.Color)
		sets = append(sets, fmt.Sprintf("color = $%d", len(args)))
	}

	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE fleet_groups SET `+strings.Join(sets, ", ")+
			` WHERE tenant_id = $1 AND id = $2`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrGroupNameTaken
		}
		return nil, fmt.Errorf("update group %s: %w", groupID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrGroupNotFound
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("group_id", groupID).Msg("fleet group updated")
	return s.Get(ctx, tenantID, groupID)
}

// Delete removes the group; memberships cascade.
func (s *GroupService) Delete(ctx context.Context, tenantID, groupID string) error {
	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`DELETE FROM fleet_groups WHERE tenant_id = $1 AND id = $2`, tenantID, groupID)
	if err != nil {
		return fmt.Errorf("delete group %s: %w", groupID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGroupNotFound
	}
	s.logger.Info().Str("tenant_id", tenantID).Str("group_id", groupID).Msg("fleet group deleted")
	return nil
}

// ListVehicles returns the token ids in a group, ordered for stable output.
func (s *GroupService) ListVehicles(ctx context.Context, tenantID, groupID string) ([]int64, error) {
	if _, err := s.Get(ctx, tenantID, groupID); err != nil {
		return nil, err
	}
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT vehicle_token_id FROM vehicle_fleet_groups
		 WHERE tenant_id = $1 AND fleet_group_id = $2
		 ORDER BY vehicle_token_id`, tenantID, groupID)
	if err != nil {
		return nil, fmt.Errorf("list vehicles of group %s: %w", groupID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan token id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddVehicles adds token ids to a group. Idempotent — re-adding is a no-op, so
// a caller retrying after a timeout cannot double anything. Only minted
// vehicles have token ids, so unminted VINs cannot arrive here by construction.
func (s *GroupService) AddVehicles(ctx context.Context, tenantID, groupID string, tokenIDs []int64) error {
	if len(tokenIDs) == 0 {
		return fmt.Errorf("%w: tokenIds is required", ErrInvalidGroupInput)
	}
	if _, err := s.Get(ctx, tenantID, groupID); err != nil {
		return err
	}
	_, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		INSERT INTO vehicle_fleet_groups (tenant_id, vehicle_token_id, fleet_group_id)
		SELECT $1, unnest($2::bigint[]), $3
		ON CONFLICT DO NOTHING`,
		tenantID, pq.Array(tokenIDs), groupID)
	if err != nil {
		return fmt.Errorf("add vehicles to group %s: %w", groupID, err)
	}
	s.logger.Info().Str("tenant_id", tenantID).Str("group_id", groupID).
		Int("token_ids", len(tokenIDs)).Msg("vehicles added to fleet group")
	return nil
}

// RemoveVehicle removes one token id from a group. Removing one already gone
// succeeds — the caller asked for a state, and it holds.
func (s *GroupService) RemoveVehicle(ctx context.Context, tenantID, groupID string, tokenID int64) error {
	if _, err := s.Get(ctx, tenantID, groupID); err != nil {
		return err
	}
	_, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		DELETE FROM vehicle_fleet_groups
		 WHERE tenant_id = $1 AND fleet_group_id = $2 AND vehicle_token_id = $3`,
		tenantID, groupID, tokenID)
	if err != nil {
		return fmt.Errorf("remove vehicle from group %s: %w", groupID, err)
	}
	s.logger.Info().Str("tenant_id", tenantID).Str("group_id", groupID).
		Int64("token_id", tokenID).Msg("vehicle removed from fleet group")
	return nil
}

func scanGroup(r rowScanner) (*models.FleetGroup, error) {
	var (
		g                    models.FleetGroup
		createdAt, updatedAt sql.NullTime
	)
	if err := r.Scan(&g.ID, &g.TenantID, &g.Name, &g.Color, &g.VehicleCount,
		&createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("scan group: %w", err)
	}
	if createdAt.Valid {
		g.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if updatedAt.Valid {
		g.UpdatedAt = updatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &g, nil
}

func isForeignKeyViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23503"
}
