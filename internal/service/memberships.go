package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

var (
	// ErrMembershipNotFound is returned for an id that names no live membership
	// of this tenant. Also returned for a malformed id: a caller that guessed
	// wrong learns nothing about which ids are real.
	ErrMembershipNotFound = errors.New("membership not found")

	// ErrVehicleNotEntitled is returned for a vehicle this tenant cannot see.
	// A membership on an unentitled vehicle is a support ticket by
	// construction, so it is refused at the point of writing rather than
	// discovered later.
	ErrVehicleNotEntitled = errors.New("that vehicle is not assigned to this customer")

	// ErrMembershipExists is returned when a vehicle already has an unexpired
	// membership. Deliberately not a silent replacement: quietly overwriting
	// paid time is the kind of bug nobody reports until an invoice is wrong.
	ErrMembershipExists = errors.New("that vehicle already has a membership")

	// ErrInvalidTerm is returned for a term that is not one of the offered ones.
	ErrInvalidTerm = errors.New("term must be 1, 12, 24, 36 or 48 months")

	// ErrInvalidStartsAt is returned for an unparseable start date.
	ErrInvalidStartsAt = errors.New("startsAt must be an RFC 3339 timestamp")

	// ErrSameVehicle is returned when a move names the vehicle the membership
	// is already on. The caller asked for a state that already obtains, but
	// saying so is better than writing a move row that records nothing.
	ErrSameVehicle = errors.New("that membership is already on this vehicle")
)

// membershipColumns is the one definition of a membership's shape on read,
// including its status.
//
// STATUS IS COMPUTED, IN SQL, ON EVERY READ. It is not a stored column and no
// job maintains it: an expiry that depends on something having run is an expiry
// that silently does not happen the day that thing breaks. Here, a membership
// is expired the moment the clock passes it, everywhere, with no way for the
// database and the application to disagree.
//
// Every interpolated value is a Go constant, so this is not a formatting hazard.
var membershipColumns = fmt.Sprintf(`
	id, vehicle_token_id, term_months, starts_at, expires_at, canceled_at,
	CASE
		WHEN canceled_at IS NOT NULL THEN '%s'
		WHEN expires_at <= NOW() THEN '%s'
		WHEN expires_at <= NOW() + make_interval(days => %d) THEN '%s'
		ELSE '%s'
	END`,
	models.MembershipCanceled,
	models.MembershipExpired,
	models.MembershipExpiringSoonDays, models.MembershipExpiringSoon,
	models.MembershipActive)

// MembershipService owns the commercial record: what a customer has paid for,
// per vehicle, and until when.
//
// It is NOT the isolation boundary — EntitlementService is. A membership
// decides whether a vehicle is paid for; the entitlement decides whether the
// customer may see it at all. That separation is why revoking a vehicle leaves
// its membership alone, and why a membership can move between vehicles without
// disturbing who can see what.
type MembershipService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewMembershipService(logger *zerolog.Logger, pdb *db.Store) *MembershipService {
	return &MembershipService{logger: logger, pdb: pdb}
}

// rowScanner is declared in tenants.go — *sql.Row and *sql.Rows both satisfy it.
func scanMembership(s rowScanner) (*models.VehicleMembership, error) {
	var (
		m          models.VehicleMembership
		startsAt   time.Time
		expiresAt  time.Time
		canceledAt sql.NullTime
	)
	if err := s.Scan(&m.ID, &m.VehicleTokenID, &m.TermMonths,
		&startsAt, &expiresAt, &canceledAt, &m.Status); err != nil {
		return nil, err
	}
	m.StartsAt = formatTime(startsAt)
	m.ExpiresAt = formatTime(expiresAt)
	if canceledAt.Valid {
		s := formatTime(canceledAt.Time)
		m.CanceledAt = &s
	}
	return &m, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// List returns a tenant's live memberships and whether enforcement is on.
//
// Canceled rows are omitted: cancelling is how a membership ends, and a list
// that accumulated them would grow without bound. Expired ones ARE included —
// they are the rows an operator most needs to see, because each one is a
// vehicle the customer has stopped being able to see.
//
// Enforced comes back on the same response so a caller never has to reconcile
// two answers that could straddle a change.
func (s *MembershipService) List(ctx context.Context, tenantID string) (*models.MembershipList, error) {
	var enforced bool
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT memberships_enforced FROM tenants WHERE id = $1`, tenantID).Scan(&enforced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}

	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT `+membershipColumns+`
		   FROM vehicle_memberships
		  WHERE tenant_id = $1 AND canceled_at IS NULL
		  ORDER BY expires_at, vehicle_token_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list memberships of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := &models.MembershipList{Enforced: enforced, Memberships: []models.VehicleMembership{}}
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		out.Memberships = append(out.Memberships, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list memberships of %s: %w", tenantID, err)
	}
	return out, nil
}

// Create starts a membership on one of the tenant's vehicles.
//
// Three preconditions, each refused rather than worked around:
//
//  1. The tenant's fleet must be defined by entitlements at all. An
//     implicit-mode tenant resolves its fleet from its licence, so there is no
//     vehicle here to attach a membership to and no console to manage it from.
//  2. The vehicle must be currently entitled to this tenant.
//  3. The vehicle must not already have an unexpired membership. If it has an
//     EXPIRED one, that row is superseded rather than blocking — a lapsed
//     vehicle has to be able to start fresh.
func (s *MembershipService) Create(ctx context.Context, tenantID string,
	in *models.CreateMembershipInput, actorWallet string) (*models.VehicleMembership, error) {
	if !models.IsValidMembershipTerm(in.TermMonths) {
		return nil, ErrInvalidTerm
	}

	startsAt := time.Now().UTC()
	if in.StartsAt != nil && *in.StartsAt != "" {
		parsed, err := time.Parse(time.RFC3339, *in.StartsAt)
		if err != nil {
			return nil, ErrInvalidStartsAt
		}
		startsAt = parsed.UTC()
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := assertExplicitMode(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	if err := assertEntitledTx(ctx, tx, tenantID, in.VehicleTokenID); err != nil {
		return nil, err
	}
	if err := supersedeOrRefuse(ctx, tx, tenantID, in.VehicleTokenID); err != nil {
		return nil, err
	}

	// expires_at is computed by Postgres from the start date, so month
	// arithmetic is the database's — make_interval clamps a 31st into a shorter
	// month, where a naive Go AddDate would roll it into the next one.
	//
	// The ::int and ::timestamptz casts are required, not decorative. Each of
	// $3 and $4 appears in two different type contexts (a SMALLINT column and
	// make_interval; a TIMESTAMPTZ column and an interval addition), and without
	// them Postgres deduces conflicting types and rejects the statement with
	// "inconsistent types deduced for parameter". That surfaces at runtime, not
	// at compile time, so it is worth leaving in place deliberately.
	row := tx.QueryRowContext(ctx,
		`INSERT INTO vehicle_memberships
		   (tenant_id, vehicle_token_id, term_months, starts_at, expires_at, created_by_wallet)
		 VALUES ($1, $2, $3::int, $4::timestamptz,
		         $4::timestamptz + make_interval(months => $3::int), NULLIF($5, ''))
		 RETURNING `+membershipColumns,
		tenantID, in.VehicleTokenID, in.TermMonths, startsAt,
		normaliseOptionalWallet(actorWallet))

	m, err := scanMembership(row)
	if err != nil {
		// The partial unique index fired: another create took this vehicle
		// between the check above and this insert. Reported as the same
		// situation the check reports, because from the caller's side it is.
		if isUniqueViolation(err) {
			return nil, ErrMembershipExists
		}
		return nil, fmt.Errorf("create membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Str("membership_id", m.ID).
		Int64("vehicle_token_id", m.VehicleTokenID).
		Int("term_months", m.TermMonths).
		Str("actor", actorWallet).
		Msg("membership created")
	return m, nil
}

// Move points a membership at a different vehicle, carrying its remaining term.
//
// This is the reason memberships are a separate record at all: a discontinued
// vehicle should not cost the customer the time they paid for. The old vehicle
// keeps its entitlement — access and payment are different questions — so
// moving a membership does not change who can see what.
func (s *MembershipService) Move(ctx context.Context, tenantID, membershipID string,
	in *models.MoveMembershipInput, actorWallet string) (*models.VehicleMembership, error) {
	if _, err := uuid.Parse(membershipID); err != nil {
		return nil, ErrMembershipNotFound
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var fromToken int64
	err = tx.QueryRowContext(ctx,
		`SELECT vehicle_token_id FROM vehicle_memberships
		  WHERE id = $1 AND tenant_id = $2 AND canceled_at IS NULL
		  FOR UPDATE`, membershipID, tenantID).Scan(&fromToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMembershipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load membership %s: %w", membershipID, err)
	}
	if fromToken == in.VehicleTokenID {
		return nil, ErrSameVehicle
	}

	if err := assertEntitledTx(ctx, tx, tenantID, in.VehicleTokenID); err != nil {
		return nil, err
	}
	if err := supersedeOrRefuse(ctx, tx, tenantID, in.VehicleTokenID); err != nil {
		return nil, err
	}

	row := tx.QueryRowContext(ctx,
		`UPDATE vehicle_memberships
		    SET vehicle_token_id = $1, updated_at = NOW()
		  WHERE id = $2
		 RETURNING `+membershipColumns, in.VehicleTokenID, membershipID)

	m, err := scanMembership(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrMembershipExists
		}
		return nil, fmt.Errorf("move membership %s: %w", membershipID, err)
	}

	// Written in the same transaction as the move it records, so the history
	// cannot end up describing a move that did not happen.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO vehicle_membership_moves
		   (membership_id, from_token_id, to_token_id, moved_by_wallet)
		 VALUES ($1, $2, $3, NULLIF($4, ''))`,
		membershipID, fromToken, in.VehicleTokenID,
		normaliseOptionalWallet(actorWallet)); err != nil {
		return nil, fmt.Errorf("record move: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Str("membership_id", membershipID).
		Int64("from_token_id", fromToken).
		Int64("to_token_id", in.VehicleTokenID).
		Str("actor", actorWallet).
		Msg("membership moved")
	return m, nil
}

// Renew extends a membership by a further term.
//
// Early renewal adds to the end of the existing term; renewal after a lapse
// starts from now. GREATEST is what makes that one expression rather than a
// branch — and it is what stops a renewal being backdated into a period the
// customer could not use the vehicle in.
//
// One row per membership, extended, rather than a stack of rows per purchase.
// A purchase history is a real thing to want, but it belongs to the purchase
// flow that does not exist yet; inventing it here would mean maintaining a
// shape nothing writes.
func (s *MembershipService) Renew(ctx context.Context, tenantID, membershipID string,
	in *models.RenewMembershipInput, actorWallet string) (*models.VehicleMembership, error) {
	if !models.IsValidMembershipTerm(in.TermMonths) {
		return nil, ErrInvalidTerm
	}
	if _, err := uuid.Parse(membershipID); err != nil {
		return nil, ErrMembershipNotFound
	}

	row := s.pdb.DBS().Writer.QueryRowContext(ctx,
		`UPDATE vehicle_memberships
		    SET expires_at  = GREATEST(expires_at, NOW()) + make_interval(months => $1::int),
		        term_months = $1::int,
		        updated_at  = NOW()
		  WHERE id = $2 AND tenant_id = $3 AND canceled_at IS NULL
		 RETURNING `+membershipColumns,
		in.TermMonths, membershipID, tenantID)

	m, err := scanMembership(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMembershipNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("renew membership %s: %w", membershipID, err)
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Str("membership_id", membershipID).
		Int("term_months", in.TermMonths).
		Str("expires_at", m.ExpiresAt).
		Str("actor", actorWallet).
		Msg("membership renewed")
	return m, nil
}

// Cancel ends a membership.
//
// Soft, mirroring entitlement revocation: the row is what a refund or a dispute
// would refer to, and a hard delete throws that away. It also frees the
// vehicle's slot under the partial unique index immediately, so a new
// membership can be started on it straight after.
func (s *MembershipService) Cancel(ctx context.Context, tenantID, membershipID string) error {
	if _, err := uuid.Parse(membershipID); err != nil {
		return ErrMembershipNotFound
	}

	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE vehicle_memberships SET canceled_at = NOW(), updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2 AND canceled_at IS NULL`,
		membershipID, tenantID)
	if err != nil {
		return fmt.Errorf("cancel membership %s: %w", membershipID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrMembershipNotFound
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("membership_id", membershipID).
		Msg("membership canceled")
	return nil
}

// ActiveTokenIDs is the read fleet-lite gates its vehicle list on.
//
// Returns the token ids with an unexpired, uncanceled membership, and whether
// enforcement is on for this tenant. Both in one call, deliberately: a caller
// that asked separately could get "enforced" from one side of a change and the
// list from the other, and the failure mode of that is a fleet that briefly
// renders empty.
//
// When enforcement is off the list is still returned rather than skipped — a
// caller may want to show membership state without filtering on it.
func (s *MembershipService) ActiveTokenIDs(ctx context.Context, tenantID string) (bool, []int64, error) {
	var enforced bool
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT memberships_enforced FROM tenants WHERE id = $1`, tenantID).Scan(&enforced)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrTenantNotFound
	}
	if err != nil {
		return false, nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}

	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT vehicle_token_id FROM vehicle_memberships
		  WHERE tenant_id = $1 AND canceled_at IS NULL AND expires_at > NOW()
		  ORDER BY vehicle_token_id`, tenantID)
	if err != nil {
		return false, nil, fmt.Errorf("active memberships of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, nil, fmt.Errorf("scan token id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("active memberships of %s: %w", tenantID, err)
	}
	return enforced, out, nil
}

// assertExplicitMode refuses a tenant whose fleet is not defined by
// entitlements, for the same reason EntitlementService.Assign does: the rows
// would be ones nothing reads.
func assertExplicitMode(ctx context.Context, tx *sql.Tx, tenantID string) error {
	var mode string
	err := tx.QueryRowContext(ctx,
		`SELECT entitlement_mode FROM tenants WHERE id = $1`, tenantID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("load tenant %s: %w", tenantID, err)
	}
	if mode != models.EntitlementExplicit {
		return ErrNotExplicitMode
	}
	return nil
}

func assertEntitledTx(ctx context.Context, tx *sql.Tx, tenantID string, tokenID int64) error {
	var ok bool
	err := tx.QueryRowContext(ctx,
		`SELECT true FROM vehicle_entitlements
		  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND revoked_at IS NULL`,
		tenantID, tokenID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVehicleNotEntitled
	}
	if err != nil {
		return fmt.Errorf("check entitlement: %w", err)
	}
	return nil
}

// supersedeOrRefuse clears the way for a membership on this vehicle.
//
// An unexpired membership refuses the write; an expired one is canceled so the
// vehicle can start fresh. FOR UPDATE holds the row for the rest of the
// transaction, so two concurrent creates on one vehicle serialise here rather
// than both passing and racing to the unique index.
func supersedeOrRefuse(ctx context.Context, tx *sql.Tx, tenantID string, tokenID int64) error {
	var (
		liveID  string
		expired bool
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, expires_at <= NOW() FROM vehicle_memberships
		  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND canceled_at IS NULL
		  FOR UPDATE`, tenantID, tokenID).Scan(&liveID, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check existing membership: %w", err)
	}
	if !expired {
		return ErrMembershipExists
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE vehicle_memberships SET canceled_at = NOW(), updated_at = NOW() WHERE id = $1`,
		liveID); err != nil {
		return fmt.Errorf("supersede lapsed membership %s: %w", liveID, err)
	}
	return nil
}
