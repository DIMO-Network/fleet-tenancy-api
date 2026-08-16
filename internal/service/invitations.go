package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

const (
	InviteStatusPending  = "pending"
	InviteStatusAccepted = "accepted"
	InviteStatusRevoked  = "revoked"

	// Email-tracking statuses. "sent" is stamped when Postmark accepts the
	// message; the /webhooks/postmark receiver upgrades it from webhook
	// events. Upgrades are monotonic (sent < delivered < opened; bounced
	// beats all) because Postmark retries and events arrive out of order.
	EmailStatusSent      = "sent"
	EmailStatusDelivered = "delivered"
	EmailStatusOpened    = "opened"
	EmailStatusBounced   = "bounced"

	defaultInviteExpiryHours = 168 // 7 days

	// The two roles an invitation can carry, unchanged from the flow this
	// ports. Role is a label; accept derives the written permissions from it.
	inviteRoleOwner  = "owner"
	inviteRoleMember = "member"
)

// ErrInviteInvalid covers every way an accept token can fail to match a usable
// invitation — unknown, superseded, revoked, already used, expired. It is
// deliberately vague so callers map it to one message without leaking which
// check failed; the real reason is logged.
var ErrInviteInvalid = errors.New("invitation is invalid, already used, or expired")

// ErrEmailNotSent signals a partial success: the invitation row was persisted
// (create) or its token refreshed (resend), but the email failed to dispatch.
// Records are authoritative and email is courtesy — the same decision as
// provisioning's access email — so this is a 201-with-flag at the controller,
// never a 5xx. The invite is usable and can be resent.
var ErrEmailNotSent = errors.New("invitation saved but the email could not be sent")

// invitationSender is the slice of gateway.PostmarkAPI this service needs,
// an interface so the flow is testable without Postmark.
type invitationSender interface {
	Enabled() bool
	SendInvitation(from, to, templateAlias string, model gateway.InvitationModel, metadata map[string]string) (string, error)
}

// InvitationService owns the email-invitation lifecycle: create (single-use
// hashed token + Postmark email), list, revoke, resend (fresh token, old link
// dies), and accept (bind the asserted wallet into the tenant). The records
// and the email dispatch both live here so the plaintext token exists in
// exactly one service's memory — it is minted, emailed and forgotten in one
// place, and only its hash is stored.
type InvitationService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	settings *config.Settings
	postmark invitationSender
}

func NewInvitationService(logger *zerolog.Logger, pdb *db.Store, settings *config.Settings, postmark invitationSender) *InvitationService {
	return &InvitationService{logger: logger, pdb: pdb, settings: settings, postmark: postmark}
}

// rolePermissions maps an invitation role onto the capability set the accepted
// membership is written with — the Q5 mapping, byte-for-byte what fleet-lite's
// write-through sends: an owner holds the two owner-gate capabilities, a plain
// member holds none. Permissions are authoritative and role is a label, so
// this mapping is the one place an invite's role becomes an authorization
// fact.
func rolePermissions(role string) []string {
	if role == inviteRoleOwner {
		return []string{models.CapManageMembers, models.CapManageSettings}
	}
	return []string{}
}

// inviteRoleRank orders role labels so accept can keep the higher one when the
// wallet already holds a membership — an accept must never demote a label
// another surface granted. Same ordering as fleet-lite's roleRank.
func inviteRoleRank(role string) int {
	switch role {
	case inviteRoleOwner:
		return 3
	case "admin":
		return 2
	case inviteRoleMember:
		return 1
	}
	return 0
}

// Create issues an invitation: it supersedes any pending invite for the same
// (tenant, email) so only one link is live, mints a single-use token (storing
// only its hash), persists the row, and dispatches the accept-link email.
//
// A send failure returns the persisted invitation together with
// ErrEmailNotSent — partial success, resendable. Anything else means nothing
// usable was persisted.
func (s *InvitationService) Create(ctx context.Context, tenantID string, in *models.InvitationCreate) (*models.Invitation, error) {
	email := strings.TrimSpace(strings.ToLower(in.Email))
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if in.InvitedByWallet == "" {
		return nil, fmt.Errorf("invitedByWallet is required")
	}
	role := in.Role
	if role != inviteRoleOwner {
		role = inviteRoleMember
	}

	groups, unrestricted, present := in.Scope()
	if !present {
		// The same explicitness as membership writes: an omitted field must be
		// a 400, not a silent grant of the whole fleet.
		return nil, fmt.Errorf("scopeGroupIds is required (null for unrestricted, [] for no groups)")
	}
	if role == inviteRoleOwner && !unrestricted {
		// Owners are always unrestricted — the same coercion the source flow
		// applies, kept loud so a caller sending groups for an owner learns it.
		s.logger.Info().Str("tenant_id", tenantID).Str("email", email).
			Msg("invite flow: owner invite forced unrestricted, provided scope ignored")
		groups, unrestricted = nil, true
	}
	if err := s.validateGroupIDs(ctx, tenantID, groups); err != nil {
		return nil, err
	}

	var exists bool
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT true FROM tenants WHERE id = $1`, tenantID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}

	// Supersede prior pending invites for this email so only one link is live.
	superseded, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE invitations SET status = $3, updated_at = NOW()
		  WHERE tenant_id = $1 AND lower(email) = $2 AND status = $4`,
		tenantID, email, InviteStatusRevoked, InviteStatusPending)
	if err != nil {
		return nil, fmt.Errorf("supersede pending invites: %w", err)
	}
	if n, _ := superseded.RowsAffected(); n > 0 {
		s.logger.Info().Str("tenant_id", tenantID).Str("email", email).Int64("count", n).
			Msg("invite flow: superseded prior pending invitations (their links are now dead)")
	}

	token, hash, err := generateInviteToken()
	if err != nil {
		return nil, err
	}

	var scopeArg any
	if !unrestricted {
		scopeArg = pq.StringArray(groups)
	}
	row := s.pdb.DBS().Writer.QueryRowContext(ctx,
		`INSERT INTO invitations
		   (tenant_id, email, role, token_hash, status, invited_by_wallet,
		    created_by_tenant_id, scope_group_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8, NOW() + $9 * INTERVAL '1 hour')
		 RETURNING `+invitationColumns,
		tenantID, email, role, hash, InviteStatusPending,
		normaliseOptionalWallet(in.InvitedByWallet), in.CreatedByTenantID,
		scopeArg, s.expiryHours())
	inv, err := scanInvitation(row)
	if err != nil {
		return nil, fmt.Errorf("insert invitation: %w", err)
	}
	s.logger.Info().Str("invitation", inv.ID).Str("tenant_id", tenantID).Str("email", email).
		Str("role", role).Str("invitedBy", inv.InvitedBy).Str("expiresAt", inv.ExpiresAt).
		Msg("invite flow: invitation created")

	messageID, err := s.sendEmail(ctx, inv, token, in.Locale)
	if err != nil {
		s.logger.Err(err).Str("invitation", inv.ID).Str("tenant_id", tenantID).Str("email", email).
			Msg("invite flow: send invitation email")
		return inv, fmt.Errorf("%w: %v", ErrEmailNotSent, err)
	}
	s.markEmailSent(ctx, inv, messageID)
	return inv, nil
}

// List returns a tenant's invitations, newest first.
func (s *InvitationService) List(ctx context.Context, tenantID string) ([]models.Invitation, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT `+invitationColumns+` FROM invitations
		  WHERE tenant_id = $1 ORDER BY created_at DESC, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list invitations of %s: %w", tenantID, err)
	}
	defer rows.Close() //nolint:errcheck

	out := []models.Invitation{}
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		out = append(out, *inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invitations of %s: %w", tenantID, err)
	}
	return out, nil
}

// Revoke marks a pending invitation revoked. Idempotent: revoking one that is
// not pending, not there, or not this tenant's is a no-op, not an error — the
// state the caller asked for already holds.
func (s *InvitationService) Revoke(ctx context.Context, tenantID, invitationID string) error {
	if _, err := uuid.Parse(invitationID); err != nil {
		return nil
	}
	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE invitations SET status = $3, updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2 AND status = $4`,
		invitationID, tenantID, InviteStatusRevoked, InviteStatusPending)
	if err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.logger.Info().Str("invitation", invitationID).Str("tenant_id", tenantID).
			Msg("invite flow: invitation revoked")
	}
	return nil
}

// Resend re-sends a pending invitation by MINTING A FRESH TOKEN — the old link
// dies, because the row's hash is replaced and an accept racing this resend
// loses by design. Email tracking is cleared first: a resend is a new message,
// and the previous one's delivery state doesn't describe it.
//
// actorWallet is who pressed resend, for the email's "invited by" line; empty
// falls back to the original inviter. Returns ErrInviteInvalid when there is
// no pending invitation to resend, ErrEmailNotSent when the token was
// refreshed but the email failed (still resendable).
func (s *InvitationService) Resend(ctx context.Context, tenantID, invitationID, actorWallet, locale string) (*models.Invitation, error) {
	if _, err := uuid.Parse(invitationID); err != nil {
		return nil, ErrInviteInvalid
	}
	token, hash, err := generateInviteToken()
	if err != nil {
		return nil, err
	}
	row := s.pdb.DBS().Writer.QueryRowContext(ctx,
		`UPDATE invitations SET
		   token_hash = $4, expires_at = NOW() + $5 * INTERVAL '1 hour', updated_at = NOW(),
		   postmark_message_id = NULL, email_status = NULL,
		   email_status_at = NULL, email_status_detail = NULL
		  WHERE id = $1 AND tenant_id = $2 AND status = $3
		 RETURNING `+invitationColumns,
		invitationID, tenantID, InviteStatusPending, hash, s.expiryHours())
	inv, err := scanInvitation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("refresh invitation token: %w", err)
	}
	s.logger.Info().Str("invitation", inv.ID).Str("tenant_id", tenantID).Str("email", inv.Email).
		Str("expiresAt", inv.ExpiresAt).
		Msg("invite flow: token refreshed for resend (previous link is now dead)")

	if actorWallet == "" {
		actorWallet = inv.InvitedBy
	}
	messageID, err := s.sendEmailAs(ctx, inv, token, locale, actorWallet)
	if err != nil {
		s.logger.Err(err).Str("invitation", inv.ID).Str("tenant_id", tenantID).Str("email", inv.Email).
			Msg("invite flow: resend invitation email")
		return inv, fmt.Errorf("%w: %v", ErrEmailNotSent, err)
	}
	s.markEmailSent(ctx, inv, messageID)
	return inv, nil
}

// Accept validates the token, writes the membership and marks the invitation
// accepted — in ONE transaction, unlike the two-step this ports, whose retry
// semantics papered over a grant that could succeed while the mark failed.
// Single-use is enforced by the row lock: a second accept of the same token
// waits, then finds the row no longer pending.
//
// authorize runs after the token resolves its tenant and before anything is
// written — it is the caller-scope check, which cannot happen earlier because
// there is no tenant in the request until the token names one. A non-nil
// return aborts the accept unwritten.
//
// The written membership MERGES with any existing one the wallet holds: union
// of permissions, the higher role label — an accept must never strip a
// capability or demote a label the console granted — while the scope is set
// verbatim from the invite, because the invite is the newer statement of what
// this person should see.
func (s *InvitationService) Accept(ctx context.Context, in *models.InvitationAccept, authorize func(tenantID string) error) (*models.Invitation, error) {
	if in.Token == "" || in.Wallet == "" {
		return nil, fmt.Errorf("token and wallet are required")
	}
	hash := hashInviteToken(in.Token)
	wallet := common.HexToAddress(in.Wallet).Hex()

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		`SELECT `+invitationColumns+` FROM invitations WHERE token_hash = $1 FOR UPDATE`, hash)
	inv, err := scanInvitation(row)
	if errors.Is(err, sql.ErrNoRows) {
		// An unknown hash means the link was superseded by a newer
		// invite/resend, or was never issued. The client answer stays vague;
		// the log says what actually happened.
		s.logger.Info().Str("wallet", wallet).Str("tokenHashPrefix", hash[:8]).
			Msg("invite flow: accept failed — token not found (superseded by a newer invite/resend, or never issued)")
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load invitation by token: %w", err)
	}

	if err := authorize(inv.TenantID); err != nil {
		return nil, err
	}

	if inv.Status != InviteStatusPending || parseWireTime(inv.ExpiresAt).Before(time.Now()) {
		s.logger.Info().Str("invitation", inv.ID).Str("tenant_id", inv.TenantID).Str("email", inv.Email).
			Str("status", inv.Status).Str("expiresAt", inv.ExpiresAt).Str("attemptingWallet", wallet).
			Msg("invite flow: accept failed — invitation not pending or expired")
		return nil, ErrInviteInvalid
	}

	write := &models.MemberWrite{
		Email:           in.Email,
		Role:            inv.Role,
		Permissions:     rolePermissions(inv.Role),
		ScopeGroupIDs:   scopeJSON(inv.ScopeGroupIDs),
		GrantedByWallet: inv.InvitedBy,
	}
	if inv.CreatedByTenantID != nil {
		write.GrantedByTenantID = *inv.CreatedByTenantID
	}
	var existingRole string
	var existingPerms []byte
	err = tx.QueryRowContext(ctx,
		`SELECT role, permissions FROM memberships
		  WHERE tenant_id = $1 AND lower(wallet) = lower($2) FOR UPDATE`,
		inv.TenantID, wallet).Scan(&existingRole, &existingPerms)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load existing membership: %w", err)
	}
	if err == nil {
		write.Permissions = unionStrings(parsePermissions(existingPerms), write.Permissions)
		if inviteRoleRank(existingRole) > inviteRoleRank(write.Role) {
			write.Role = existingRole
		}
	}

	if _, err := upsertMemberTx(ctx, tx, inv.TenantID, wallet, write); err != nil {
		return nil, fmt.Errorf("write membership for accept: %w", err)
	}

	row = tx.QueryRowContext(ctx,
		`UPDATE invitations SET status = $2, invitee_wallet = $3,
		        accepted_at = NOW(), updated_at = NOW()
		  WHERE id = $1 RETURNING `+invitationColumns,
		inv.ID, InviteStatusAccepted, wallet)
	accepted, err := scanInvitation(row)
	if err != nil {
		return nil, fmt.Errorf("mark invitation accepted: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit accept: %w", err)
	}
	s.logger.Info().Str("invitation", accepted.ID).Str("tenant_id", accepted.TenantID).
		Str("email", accepted.Email).Str("role", write.Role).Str("inviteeWallet", wallet).
		Msg("invite flow: invitation accepted, membership written")
	return accepted, nil
}

// emailStatusRank orders email-tracking statuses for monotonic upgrades. A
// status may only replace one with a strictly lower rank; unknown/empty ranks
// lowest so any real status wins.
func emailStatusRank(status string) int {
	switch status {
	case EmailStatusSent:
		return 1
	case EmailStatusDelivered:
		return 2
	case EmailStatusOpened:
		return 3
	case EmailStatusBounced:
		return 4
	default:
		return 0
	}
}

// ApplyEmailEvent records a Postmark webhook event (delivered/opened/bounced)
// against the invitation it belongs to, resolved by invitation id from the
// message metadata with the Postmark message id as fallback — copy BOTH in the
// backfill or historical bounces stop resolving. Unknown invitations and
// out-of-order or duplicate events are swallowed: the webhook must 200 either
// way so Postmark stops retrying events that can never apply.
func (s *InvitationService) ApplyEmailEvent(ctx context.Context, invitationID, messageID, status string, occurredAt time.Time, detail string) error {
	if _, err := uuid.Parse(invitationID); err != nil {
		// Metadata absent or not ours; fall back to the message id.
		invitationID = ""
	}
	if invitationID == "" && messageID == "" {
		return nil
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	// The rank comparison rides in the WHERE clause, so a duplicate or
	// out-of-order event updates zero rows rather than racing a read.
	q := `UPDATE invitations SET email_status = $1, email_status_at = $2,
	             email_status_detail = NULLIF($3, ''), updated_at = NOW()
	       WHERE `
	args := []any{status, occurredAt, detail}
	if invitationID != "" {
		q += `id = $4`
		args = append(args, invitationID)
	} else {
		q += `postmark_message_id = $4`
		args = append(args, messageID)
	}
	q += ` AND CASE COALESCE(email_status, '')
	         WHEN 'sent' THEN 1 WHEN 'delivered' THEN 2
	         WHEN 'opened' THEN 3 WHEN 'bounced' THEN 4 ELSE 0 END
	       < CASE $1 WHEN 'sent' THEN 1 WHEN 'delivered' THEN 2
	         WHEN 'opened' THEN 3 WHEN 'bounced' THEN 4 ELSE 0 END`
	res, err := s.pdb.DBS().Writer.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("record email event: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.logger.Info().Str("invitation", invitationID).Str("messageId", messageID).
			Str("status", status).Str("detail", detail).
			Msg("invite flow: email " + status)
	}
	return nil
}

// markEmailSent stamps the Postmark message id + email_status='sent' after a
// successful dispatch. Best-effort: tracking is advisory, so a failure is
// logged, never surfaced. No-op when sending is disabled (empty messageID).
func (s *InvitationService) markEmailSent(ctx context.Context, inv *models.Invitation, messageID string) {
	if messageID == "" {
		return
	}
	if _, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE invitations SET postmark_message_id = $2, email_status = $3,
		        email_status_at = NOW(), email_status_detail = NULL, updated_at = NOW()
		  WHERE id = $1`,
		inv.ID, messageID, EmailStatusSent); err != nil {
		s.logger.Warn().Err(err).Str("invitation", inv.ID).Str("messageId", messageID).
			Msg("invite flow: could not record email tracking status")
		return
	}
	st := EmailStatusSent
	inv.EmailStatus = &st
}

// validateGroupIDs verifies every id names one of the tenant's fleet groups —
// against this service's own tables, which is the point of the move: the
// authority validates against itself, not a mirror. nil and empty pass
// trivially (unrestricted, and restricted-to-nothing).
func (s *InvitationService) validateGroupIDs(ctx context.Context, tenantID string, groupIDs []string) error {
	if len(groupIDs) == 0 {
		return nil
	}
	distinct := map[string]struct{}{}
	for _, id := range groupIDs {
		distinct[id] = struct{}{}
	}
	var n int
	err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT count(*) FROM fleet_groups WHERE tenant_id = $1 AND id = ANY($2)`,
		tenantID, pq.StringArray(groupIDs)).Scan(&n)
	if err != nil {
		return fmt.Errorf("validate group ids: %w", err)
	}
	if n != len(distinct) {
		return fmt.Errorf("one or more group ids do not exist in this tenant")
	}
	return nil
}

// sendEmail dispatches the accept-link email, attributed to the original
// inviter.
func (s *InvitationService) sendEmail(ctx context.Context, inv *models.Invitation, token, locale string) (string, error) {
	return s.sendEmailAs(ctx, inv, token, locale, inv.InvitedBy)
}

// sendEmailAs builds the accept link + template model and dispatches via
// Postmark, picking the template whose language matches the locale. The
// invitation id rides along as message metadata so webhook events correlate
// back to the row; the returned MessageID ("" when sending is disabled) is the
// secondary correlation key.
func (s *InvitationService) sendEmailAs(ctx context.Context, inv *models.Invitation, token, locale, inviterLabel string) (string, error) {
	from := s.settings.InvitationFromEmail
	if from == "" {
		from = s.settings.ProvisionEmailFrom
	}
	tenantName := inv.TenantID
	var name string
	if err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT name FROM tenants WHERE id = $1`, inv.TenantID).Scan(&name); err == nil && name != "" {
		tenantName = name
	}
	model := gateway.InvitationModel{
		TenantName: tenantName,
		AcceptURL:  s.acceptURL(token),
		Inviter:    inviterLabel,
		ExpiresIn:  s.expiryLabel(locale),
	}
	alias := s.templateAlias(locale)
	messageID, err := s.postmark.SendInvitation(from, inv.Email, alias, model,
		map[string]string{"invitation_id": inv.ID})
	if err != nil {
		return "", err
	}
	s.logger.Info().Str("invitation", inv.ID).Str("tenant_id", inv.TenantID).Str("email", inv.Email).
		Str("template", alias).Str("messageId", messageID).
		Msg("invite flow: invitation email dispatched")
	return messageID, nil
}

const defaultInvitationTemplateAlias = "fleet-invitation"

// templateAlias maps a locale to the Postmark template alias: the configured
// base for English (the default), "-es" appended for Spanish — fleet-lite's
// convention, because the aliases ARE fleet-lite's, living server-side in the
// shared Postmark server. Only this config moved.
func (s *InvitationService) templateAlias(locale string) string {
	base := s.settings.InvitationTemplateAlias
	if base == "" {
		base = defaultInvitationTemplateAlias
	}
	if isSpanish(locale) {
		return base + "-es"
	}
	return base
}

// isSpanish collapses a locale tag to the shipped-template question: anything
// not Spanish (empty, "en", region tags) resolves to English.
func isSpanish(locale string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "es")
}

func (s *InvitationService) expiryHours() int {
	if h := s.settings.InviteExpiryHours; h > 0 {
		return h
	}
	return defaultInviteExpiryHours
}

// expiryLabel renders the token lifetime as human copy for the {{expires_in}}
// template variable, e.g. "7 days" / "7 días".
func (s *InvitationService) expiryLabel(locale string) string {
	h := s.expiryHours()
	es := isSpanish(locale)
	if h%24 == 0 {
		days := h / 24
		switch {
		case days == 1 && es:
			return "1 día"
		case days == 1:
			return "1 day"
		case es:
			return fmt.Sprintf("%d días", days)
		default:
			return fmt.Sprintf("%d days", days)
		}
	}
	if es {
		return fmt.Sprintf("%d horas", h)
	}
	return fmt.Sprintf("%d hours", h)
}

// acceptURL builds the public accept link the email points at:
//
//	{INVITE_ACCEPT_URL_BASE}/accept-invite.html?token=<token>
//
// The base points at fleet-lite (fleets.dimo.co): every accept happens there
// regardless of who sent the invite — operator-sent invites link to the same
// page.
func (s *InvitationService) acceptURL(token string) string {
	base := s.settings.InviteAcceptURLBase
	base.Path = strings.TrimRight(base.Path, "/") + "/accept-invite.html"
	q := url.Values{}
	q.Set("token", token)
	base.RawQuery = q.Encode()
	return base.String()
}

// generateInviteToken returns a URL-safe random token and its SHA-256 hash.
// Only the hash is persisted — the plaintext exists in the email link and
// nowhere else, so it must never appear in logs, list responses, or the
// backfill.
func generateInviteToken() (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate invite token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashInviteToken(token), nil
}

func hashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// scopeJSON re-encodes a scanned scope into the raw tri-state a MemberWrite
// carries, preserving nil-vs-empty: the invite's scope becomes the
// membership's scope verbatim.
func scopeJSON(scope []string) json.RawMessage {
	if scope == nil {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(scope)
	return b
}

// invitationColumns is the one SELECT/RETURNING list every invitation read
// uses, so scanInvitation cannot drift from the queries that feed it.
const invitationColumns = `id, tenant_id, email, role, status, invited_by_wallet,
	invitee_wallet, created_by_tenant_id, scope_group_ids,
	email_status, email_status_at, email_status_detail,
	created_at, expires_at, accepted_at`

// rowScanner is declared in tenants.go — *sql.Row and *sql.Rows both satisfy it.
func scanInvitation(row rowScanner) (*models.Invitation, error) {
	var (
		inv         models.Invitation
		invitee     sql.NullString
		createdBy   sql.NullString
		scope       pq.StringArray
		emailStatus sql.NullString
		emailAt     sql.NullTime
		emailDetail sql.NullString
		createdAt   time.Time
		expiresAt   time.Time
		acceptedAt  sql.NullTime
	)
	if err := row.Scan(&inv.ID, &inv.TenantID, &inv.Email, &inv.Role, &inv.Status,
		&inv.InvitedBy, &invitee, &createdBy, &scope,
		&emailStatus, &emailAt, &emailDetail,
		&createdAt, &expiresAt, &acceptedAt); err != nil {
		return nil, err
	}
	inv.InviteeWallet = nullStringPtr(invitee)
	inv.CreatedByTenantID = nullStringPtr(createdBy)
	// nil stays nil (unrestricted); empty stays empty (restricted to nothing).
	if scope != nil {
		inv.ScopeGroupIDs = []string(scope)
	}
	inv.EmailStatus = nullStringPtr(emailStatus)
	if emailAt.Valid {
		s := wireTime(emailAt.Time)
		inv.EmailStatusAt = &s
	}
	inv.EmailStatusDetail = nullStringPtr(emailDetail)
	inv.CreatedAt = wireTime(createdAt)
	inv.ExpiresAt = wireTime(expiresAt)
	if acceptedAt.Valid {
		s := wireTime(acceptedAt.Time)
		inv.AcceptedAt = &s
	}
	return &inv, nil
}

func wireTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseWireTime reads back what wireTime wrote; zero time on failure, which
// for an expiry check fails closed (a zero time is always in the past).
func parseWireTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// unionStrings merges two capability sets preserving first-seen order.
func unionStrings(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}
