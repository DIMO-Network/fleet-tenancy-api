package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// ErrMemberNotFound is returned when removing a membership that is not there.
var ErrMemberNotFound = errors.New("membership not found")

// MemberService writes memberships. Reads live in AuthzService.
type MemberService struct {
	logger *zerolog.Logger
	pdb    *db.Store
}

func NewMemberService(logger *zerolog.Logger, pdb *db.Store) *MemberService {
	return &MemberService{logger: logger, pdb: pdb}
}

// Upsert creates or replaces one membership.
//
// REPLACES, not merges. permissions and scope arrive whole and overwrite what
// was there, so removing a capability at the caller removes it here. A merge
// would accumulate and could never shed one — the same reasoning that kept the
// backfill's merge in memory instead of in ON CONFLICT.
//
// The users row is upserted first because memberships.wallet references it. Both
// happen in one transaction: a users row with no membership is harmless, but a
// half-applied grant that leaves the caller believing it succeeded is not.
func (s *MemberService) Upsert(ctx context.Context, tenantID, wallet string, in *models.MemberWrite) error {
	if tenantID == "" || wallet == "" {
		return fmt.Errorf("tenantID and wallet are required")
	}
	groups, unrestricted, present := in.Scope()
	if !present {
		return fmt.Errorf("scopeGroupIds is required (null for unrestricted, [] for no groups)")
	}

	// Stored EIP-55 checksummed, so callers that disagree on casing — kaufmann
	// checksums, fleet-lite lowercases — still land on one row per person.
	checksummed := common.HexToAddress(wallet).Hex()

	role := in.Role
	if role == "" {
		role = "member"
	}
	perms := in.Permissions
	if perms == nil {
		perms = []string{}
	}
	permsJSON, err := json.Marshal(perms)
	if err != nil {
		return fmt.Errorf("marshal permissions: %w", err)
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM tenants WHERE id = $1`, tenantID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("load tenant %s: %w", tenantID, err)
	}

	// COALESCE keeps an existing email when the caller sends none: knowing a
	// wallet is not the same as knowing it has no address.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO users (wallet, email) VALUES ($1, NULLIF($2, ''))
		 ON CONFLICT (wallet) DO UPDATE SET
		   email = COALESCE(NULLIF(EXCLUDED.email, ''), users.email),
		   updated_at = NOW()`,
		checksummed, in.Email); err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	// scope_group_ids is NULL for unrestricted and a (possibly empty) array
	// otherwise. pq.StringArray of an empty slice writes '{}', which is the
	// restrictive answer — not the same as NULL, and the difference is the
	// whole point of the three-valued input.
	var scopeArg any
	if unrestricted {
		scopeArg = nil
	} else {
		scopeArg = pq.StringArray(groups)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO memberships
		   (tenant_id, wallet, role, permissions, scope_group_ids, granted_by_wallet, granted_by_tenant_id)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::uuid)
		 ON CONFLICT (tenant_id, wallet) DO UPDATE SET
		   role                 = EXCLUDED.role,
		   permissions          = EXCLUDED.permissions,
		   scope_group_ids      = EXCLUDED.scope_group_ids,
		   granted_by_wallet    = COALESCE(EXCLUDED.granted_by_wallet, memberships.granted_by_wallet),
		   granted_by_tenant_id = COALESCE(EXCLUDED.granted_by_tenant_id, memberships.granted_by_tenant_id),
		   updated_at           = NOW()`,
		tenantID, checksummed, role, permsJSON, scopeArg,
		normaliseOptionalWallet(in.GrantedByWallet), in.GrantedByTenantID); err != nil {
		return fmt.Errorf("upsert membership: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Str("wallet", checksummed).
		Str("role", role).
		Int("permissions", len(perms)).
		Bool("unrestricted", unrestricted).
		Msg("membership written")
	return nil
}

// Get returns one membership in the same wire shape ListMembers uses, so a
// caller that just wrote a member can hand back exactly what a subsequent list
// would show — the write path and the read path cannot drift apart.
func (s *MemberService) Get(ctx context.Context, tenantID, wallet string) (*models.Member, error) {
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
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT m.wallet, u.email, m.role, m.permissions, m.scope_group_ids,
		        m.granted_by_tenant_id, m.granted_by_wallet, m.last_login_at, m.created_at
		   FROM memberships m
		   LEFT JOIN users u ON u.wallet = m.wallet
		  WHERE m.tenant_id = $1 AND lower(m.wallet) = lower($2)`,
		tenantID, wallet).Scan(&m.Wallet, &email, &m.Role, &permsJSON, &scopeGroups,
		&grantedTen, &grantedWal, &lastLogin, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMemberNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get member %s of %s: %w", wallet, tenantID, err)
	}
	m.Email = nullStringPtr(email)
	m.Permissions = parsePermissions(permsJSON)
	// nil stays nil (unrestricted); empty stays empty (restricted to nothing).
	if scopeGroups != nil {
		m.ScopeGroupIDs = []string(scopeGroups)
	}
	m.GrantedByTenantID = nullStringPtr(grantedTen)
	m.GrantedByWallet = nullStringPtr(grantedWal)
	m.LastLoginAt = nullTimePtr(lastLogin)
	if createdAt.Valid {
		m.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return &m, nil
}

// Remove deletes a membership.
//
// A hard delete, matching what the shared model means by "not a member". The
// caller's own tables may prefer a soft revoke — kaufmann flips is_admin rather
// than deleting — but here the row's only job is to answer "may this wallet
// act", and a row that answers "no" is indistinguishable from no row while
// making every query carry the distinction.
func (s *MemberService) Remove(ctx context.Context, tenantID, wallet string) error {
	if tenantID == "" || wallet == "" {
		return fmt.Errorf("tenantID and wallet are required")
	}
	checksummed := common.HexToAddress(wallet).Hex()

	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`DELETE FROM memberships WHERE tenant_id = $1 AND lower(wallet) = lower($2)`,
		tenantID, checksummed)
	if err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrMemberNotFound
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("wallet", checksummed).
		Msg("membership removed")
	return nil
}

// TouchLogin stamps a member's last_login_at, and fills the users row's email
// only when it has none. Reported as found/not-found via the bool rather than
// an error: a missing row here usually means the member was revoked inside the
// authz cache window, which the caller treats as telemetry lost, not a fault.
//
// The fill-if-empty rule for email is deliberate, and different from
// fleet-lite's local behaviour (which overwrites). The users row is shared
// identity across every tenant the person belongs to, and the login email is
// client-supplied — provisioning's email is the better source, so a login only
// ever adds knowledge where there was none.
func (s *MemberService) TouchLogin(ctx context.Context, tenantID, wallet, email string) (bool, error) {
	if tenantID == "" || wallet == "" {
		return false, fmt.Errorf("tenantID and wallet are required")
	}

	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE memberships SET last_login_at = NOW(), updated_at = NOW()
		  WHERE tenant_id = $1 AND lower(wallet) = lower($2)`,
		tenantID, wallet)
	if err != nil {
		return false, fmt.Errorf("touch login: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	if email != "" {
		if _, err := s.pdb.DBS().Writer.ExecContext(ctx,
			`UPDATE users SET email = $1, updated_at = NOW()
			  WHERE lower(wallet) = lower($2) AND COALESCE(email, '') = ''`,
			email, wallet); err != nil {
			// The stamp is what the caller came for and it is already written.
			s.logger.Warn().Err(err).Str("wallet", wallet).
				Msg("touch login: could not fill user email")
		}
	}
	return true, nil
}

// normaliseOptionalWallet checksums a wallet, leaving the empty string empty so
// NULLIF still sees it. common.HexToAddress("") returns the zero address, which
// would otherwise be recorded as a real actor.
func normaliseOptionalWallet(w string) string {
	if w == "" {
		return ""
	}
	return common.HexToAddress(w).Hex()
}
