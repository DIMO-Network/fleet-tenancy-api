package service

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wallets the shared fixtures don't use, so these tests write freely without
// disturbing the authz fixtures in the same database.
const (
	inviteeWallet = "0x6666666666666666666666666666666666666666"
	inviterWallet = "0x7777777777777777777777777777777777777777"
)

// stubInviteSender captures what would have gone to Postmark. The accept URL
// it records is how tests learn the plaintext token — exactly the position the
// real invitee is in, since the token exists nowhere else.
type stubInviteSender struct {
	fail      bool
	messageID string
	sends     int
	lastFrom  string
	lastTo    string
	lastAlias string
	lastModel gateway.InvitationModel
	lastMeta  map[string]string
}

func (s *stubInviteSender) Enabled() bool { return true }

func (s *stubInviteSender) SendInvitation(from, to, alias string, model gateway.InvitationModel, meta map[string]string) (string, error) {
	if s.fail {
		return "", fmt.Errorf("postmark is down")
	}
	s.sends++
	s.lastFrom, s.lastTo, s.lastAlias, s.lastModel, s.lastMeta = from, to, alias, model, meta
	return s.messageID, nil
}

// token digs the plaintext token back out of the recorded accept link.
func (s *stubInviteSender) token(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(s.lastModel.AcceptURL)
	require.NoError(t, err)
	tok := u.Query().Get("token")
	require.NotEmpty(t, tok, "accept URL must carry the token")
	return tok
}

func inviteSettings() *config.Settings {
	return &config.Settings{
		InvitationFromEmail:     "invites@example.com",
		InvitationTemplateAlias: "fleet-invitation",
		InviteExpiryHours:       48,
		InviteAcceptURLBase:     url.URL{Scheme: "https", Host: "fleets.example.com"},
	}
}

func invitationFixture(t *testing.T) (*InvitationService, *stubInviteSender, *AuthzService, func() *stubInviteSender) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	sender := &stubInviteSender{messageID: "pm-1"}
	settings := inviteSettings()
	svc := NewInvitationService(&l, store, settings, sender)
	authz := NewAuthzService(&l, store)

	w := store.DBS().Writer
	_, err := w.Exec(`INSERT INTO fleet_groups (id, tenant_id, name, color)
		VALUES ($1, $2, 'Vans', '#112233')
		ON CONFLICT (id) DO NOTHING`, opTenant+"_vans", opTenant)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Invitations cascade when seed() deletes the fixture tenants, but this
		// test run's own rows should not leak into the next.
		_, _ = w.Exec(`DELETE FROM invitations WHERE tenant_id = ANY($1)`,
			"{"+opTenant+","+custTenant+"}")
		_, _ = w.Exec(`DELETE FROM memberships WHERE wallet = ANY($1)`, "{"+inviteeWallet+"}")
		_, _ = w.Exec(`DELETE FROM users WHERE wallet = ANY($1)`, "{"+inviteeWallet+","+inviterWallet+"}")
		_, _ = w.Exec(`DELETE FROM fleet_groups WHERE id = $1`, opTenant+"_vans")
	})

	// swap lets a subtest install a fresh sender so recorded state is its own.
	swap := func() *stubInviteSender {
		fresh := &stubInviteSender{messageID: "pm-1"}
		svc.postmark = fresh
		return fresh
	}
	return svc, sender, authz, swap
}

func allowAll(string) error { return nil }

func TestInvitationCreate(t *testing.T) {
	svc, sender, _, _ := invitationFixture(t)
	ctx := context.Background()

	t.Run("input the service must refuse", func(t *testing.T) {
		_, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
		assert.ErrorContains(t, err, "email is required")

		_, err = svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "a@b.co", ScopeGroupIDs: scope("null")})
		assert.ErrorContains(t, err, "invitedByWallet is required")

		// An omitted scope must be an error, not a silent grant of everything.
		_, err = svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "a@b.co", InvitedByWallet: inviterWallet})
		assert.ErrorContains(t, err, "scopeGroupIds is required")

		_, err = svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "a@b.co", InvitedByWallet: inviterWallet,
			ScopeGroupIDs: scope(`["` + opTenant + `_nope"]`)})
		assert.ErrorContains(t, err, "group ids do not exist")
	})

	t.Run("create persists, sends, and stamps tracking", func(t *testing.T) {
		inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "  Invitee@Example.COM ", Role: "member", Locale: "en",
			InvitedByWallet: inviterWallet, ScopeGroupIDs: scope(`["` + opTenant + `_vans"]`)})
		require.NoError(t, err)

		assert.Equal(t, "invitee@example.com", inv.Email, "email normalises")
		assert.Equal(t, InviteStatusPending, inv.Status)
		assert.Equal(t, []string{opTenant + "_vans"}, inv.ScopeGroupIDs)
		assert.Nil(t, inv.CreatedByTenantID, "self-sent invite carries no issuing tenant")
		require.NotNil(t, inv.EmailStatus)
		assert.Equal(t, EmailStatusSent, *inv.EmailStatus)

		assert.Equal(t, "invites@example.com", sender.lastFrom)
		assert.Equal(t, "invitee@example.com", sender.lastTo)
		assert.Equal(t, "fleet-invitation", sender.lastAlias)
		assert.Equal(t, map[string]string{"invitation_id": inv.ID}, sender.lastMeta,
			"the invitation id must ride as metadata for webhook correlation")
		assert.Equal(t, "Op", sender.lastModel.TenantName)
		assert.Equal(t, "2 days", sender.lastModel.ExpiresIn)

		// The token is the credential: the plaintext lives only in the accept
		// link, and what the database holds is its SHA-256, not the token.
		tok := sender.token(t)
		assert.Equal(t, hashInviteToken(tok), storedTokenHash(t, svc, inv.ID))
		assert.NotEqual(t, tok, storedTokenHash(t, svc, inv.ID))
	})

	t.Run("spanish locale picks the -es alias", func(t *testing.T) {
		_, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "es@example.com", Locale: "es-CL",
			InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
		require.NoError(t, err)
		assert.Equal(t, "fleet-invitation-es", sender.lastAlias)
		assert.Equal(t, "2 días", sender.lastModel.ExpiresIn)
	})

	t.Run("a new invite supersedes the pending one for the same email", func(t *testing.T) {
		first, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "twice@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
		require.NoError(t, err)
		second, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "TWICE@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
		require.NoError(t, err)

		byID := listByID(t, svc, opTenant)
		assert.Equal(t, InviteStatusRevoked, byID[first.ID].Status, "only one link may be live")
		assert.Equal(t, InviteStatusPending, byID[second.ID].Status)
	})

	t.Run("owner invites are always unrestricted", func(t *testing.T) {
		inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "owner@example.com", Role: "owner",
			InvitedByWallet: inviterWallet, ScopeGroupIDs: scope(`["` + opTenant + `_vans"]`)})
		require.NoError(t, err)
		assert.Nil(t, inv.ScopeGroupIDs, "owner scope is forced unrestricted")
	})

	t.Run("email failure is partial success", func(t *testing.T) {
		sender.fail = true
		defer func() { sender.fail = false }()
		inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: "unsent@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
		require.ErrorIs(t, err, ErrEmailNotSent)
		require.NotNil(t, inv, "the record is authoritative; the email is courtesy")
		assert.Equal(t, InviteStatusPending, inv.Status)
		assert.Nil(t, inv.EmailStatus, "nothing dispatched, nothing stamped")
	})
}

func TestInvitationAccept(t *testing.T) {
	svc, _, authz, freshSender := invitationFixture(t)
	ctx := context.Background()

	create := func(t *testing.T, email, role, scopeRaw string) (inv *models.Invitation, token string) {
		t.Helper()
		sender := freshSender()
		inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
			Email: email, Role: role, InvitedByWallet: inviterWallet,
			ScopeGroupIDs: scope(scopeRaw)})
		require.NoError(t, err)
		return inv, sender.token(t)
	}

	t.Run("accept grants the membership and consumes the token", func(t *testing.T) {
		inv, token := create(t, "member@example.com", "member", `["`+opTenant+`_vans"]`)

		accepted, err := svc.Accept(ctx, &models.InvitationAccept{
			Token: token, Wallet: inviteeWallet, Email: "member@example.com"}, allowAll)
		require.NoError(t, err)
		assert.Equal(t, InviteStatusAccepted, accepted.Status)
		require.NotNil(t, accepted.InviteeWallet)
		assert.Equal(t, inviteeWallet, *accepted.InviteeWallet)
		assert.NotNil(t, accepted.AcceptedAt)
		assert.Equal(t, inv.ID, accepted.ID)

		got, err := authz.Authorize(ctx, opTenant, inviteeWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.Equal(t, "member", got.Role)
		assert.Empty(t, got.Permissions, "a plain member holds no capabilities")
		assert.Equal(t, []string{opTenant + "_vans"}, got.ScopeGroupIDs,
			"the invite's scope becomes the membership's scope verbatim")

		// Single-use: the same token a second time answers the vague error.
		_, err = svc.Accept(ctx, &models.InvitationAccept{Token: token, Wallet: inviteeWallet}, allowAll)
		assert.ErrorIs(t, err, ErrInviteInvalid)

		removeMembership(t, svc, opTenant, inviteeWallet)
	})

	t.Run("owner accept carries the Q5 capability mapping", func(t *testing.T) {
		_, token := create(t, "owner2@example.com", "owner", "null")
		_, err := svc.Accept(ctx, &models.InvitationAccept{Token: token, Wallet: inviteeWallet}, allowAll)
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, opTenant, inviteeWallet)
		require.NoError(t, err)
		assert.Equal(t, "owner", got.Role)
		assert.ElementsMatch(t, []string{models.CapManageMembers, models.CapManageSettings}, got.Permissions)
		assert.True(t, got.Unrestricted())

		removeMembership(t, svc, opTenant, inviteeWallet)
	})

	t.Run("accept merges with an existing membership instead of clobbering it", func(t *testing.T) {
		// The console granted this wallet admin + onboard_vehicles; accepting a
		// member invite must not strip either, while the scope IS the invite's.
		l := zerolog.Nop()
		members := NewMemberService(&l, svc.pdb)
		require.NoError(t, members.Upsert(ctx, opTenant, inviteeWallet, &models.MemberWrite{
			Role: "admin", Permissions: []string{"onboard_vehicles"}, ScopeGroupIDs: scope("null")}))

		_, token := create(t, "existing@example.com", "member", `[]`)
		_, err := svc.Accept(ctx, &models.InvitationAccept{Token: token, Wallet: inviteeWallet}, allowAll)
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, opTenant, inviteeWallet)
		require.NoError(t, err)
		assert.Equal(t, "admin", got.Role, "an accept never demotes a label another surface granted")
		assert.Contains(t, got.Permissions, "onboard_vehicles", "an accept never strips a capability")
		assert.False(t, got.Unrestricted())
		assert.Empty(t, got.ScopeGroupIDs, "the scope is set verbatim — [] means sees nothing")
		assert.NotNil(t, got.ScopeGroupIDs)

		removeMembership(t, svc, opTenant, inviteeWallet)
	})

	t.Run("a refused authorize leaves everything unwritten", func(t *testing.T) {
		inv, token := create(t, "refused@example.com", "member", "null")
		boom := fmt.Errorf("caller may not act on this tenant")
		_, err := svc.Accept(ctx, &models.InvitationAccept{Token: token, Wallet: inviteeWallet},
			func(tenantID string) error {
				assert.Equal(t, opTenant, tenantID, "authorize sees the tenant the token resolved")
				return boom
			})
		require.ErrorIs(t, err, boom)

		got, err := authz.Authorize(ctx, opTenant, inviteeWallet)
		require.NoError(t, err)
		assert.False(t, got.Member, "no membership may be written on a refused accept")
		assert.Equal(t, InviteStatusPending, listByID(t, svc, opTenant)[inv.ID].Status,
			"the invitation survives to be accepted through an authorized caller")
	})

	t.Run("expired and revoked tokens answer the same vague error", func(t *testing.T) {
		inv, token := create(t, "expired@example.com", "member", "null")
		_, err := svc.pdb.DBS().Writer.Exec(
			`UPDATE invitations SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, inv.ID)
		require.NoError(t, err)
		_, err = svc.Accept(ctx, &models.InvitationAccept{Token: token, Wallet: inviteeWallet}, allowAll)
		assert.ErrorIs(t, err, ErrInviteInvalid)

		inv2, token2 := create(t, "revoked@example.com", "member", "null")
		require.NoError(t, svc.Revoke(ctx, opTenant, inv2.ID))
		_, err = svc.Accept(ctx, &models.InvitationAccept{Token: token2, Wallet: inviteeWallet}, allowAll)
		assert.ErrorIs(t, err, ErrInviteInvalid)
	})
}

func TestInvitationResend(t *testing.T) {
	svc, sender, _, freshSender := invitationFixture(t)
	ctx := context.Background()

	inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
		Email: "resend@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
	require.NoError(t, err)
	oldToken := sender.token(t)

	t.Run("resend mints a fresh token and the old link dies", func(t *testing.T) {
		resendSender := freshSender()
		resendSender.messageID = "pm-2"
		got, err := svc.Resend(ctx, opTenant, inv.ID, "", "en")
		require.NoError(t, err)
		require.NotNil(t, got.EmailStatus)
		assert.Equal(t, EmailStatusSent, *got.EmailStatus)
		assert.Equal(t, inviterWallet, resendSender.lastModel.Inviter,
			"no actor given falls back to the original inviter (checksummed)")

		newToken := resendSender.token(t)
		assert.NotEqual(t, oldToken, newToken)

		_, err = svc.Accept(ctx, &models.InvitationAccept{Token: oldToken, Wallet: inviteeWallet}, allowAll)
		assert.ErrorIs(t, err, ErrInviteInvalid, "an accept racing a resend loses, by design")

		_, err = svc.Accept(ctx, &models.InvitationAccept{Token: newToken, Wallet: inviteeWallet}, allowAll)
		require.NoError(t, err, "the fresh link works")
		removeMembership(t, svc, opTenant, inviteeWallet)
	})

	t.Run("resending what is not pending is ErrInviteInvalid", func(t *testing.T) {
		_, err := svc.Resend(ctx, opTenant, inv.ID, "", "")
		assert.ErrorIs(t, err, ErrInviteInvalid, "the invite was just accepted")
		_, err = svc.Resend(ctx, opTenant, "not-a-uuid", "", "")
		assert.ErrorIs(t, err, ErrInviteInvalid)
	})
}

func TestInvitationRevokeIsIdempotent(t *testing.T) {
	svc, _, _, _ := invitationFixture(t)
	ctx := context.Background()

	inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
		Email: "revoke@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
	require.NoError(t, err)

	require.NoError(t, svc.Revoke(ctx, opTenant, inv.ID))
	assert.Equal(t, InviteStatusRevoked, listByID(t, svc, opTenant)[inv.ID].Status)
	// Again, from another tenant, and with garbage — all no-ops, never errors.
	require.NoError(t, svc.Revoke(ctx, opTenant, inv.ID))
	require.NoError(t, svc.Revoke(ctx, custTenant, inv.ID))
	require.NoError(t, svc.Revoke(ctx, opTenant, "not-a-uuid"))
	assert.Equal(t, InviteStatusRevoked, listByID(t, svc, opTenant)[inv.ID].Status,
		"a foreign tenant's revoke must not have touched the row")
}

func TestApplyEmailEventIsMonotonic(t *testing.T) {
	svc, _, _, _ := invitationFixture(t)
	ctx := context.Background()

	inv, err := svc.Create(ctx, opTenant, &models.InvitationCreate{
		Email: "track@example.com", InvitedByWallet: inviterWallet, ScopeGroupIDs: scope("null")})
	require.NoError(t, err)

	status := func() string {
		row := listByID(t, svc, opTenant)[inv.ID]
		if row.EmailStatus == nil {
			return ""
		}
		return *row.EmailStatus
	}
	require.Equal(t, EmailStatusSent, status(), "create stamped the dispatch")

	require.NoError(t, svc.ApplyEmailEvent(ctx, inv.ID, "", EmailStatusDelivered, time.Now(), ""))
	assert.Equal(t, EmailStatusDelivered, status())

	// Out-of-order and duplicate events must not downgrade.
	require.NoError(t, svc.ApplyEmailEvent(ctx, inv.ID, "", EmailStatusSent, time.Now(), ""))
	assert.Equal(t, EmailStatusDelivered, status())

	// Resolution falls back to the Postmark message id when metadata is absent.
	require.NoError(t, svc.ApplyEmailEvent(ctx, "", "pm-1", EmailStatusOpened, time.Now(), ""))
	assert.Equal(t, EmailStatusOpened, status())

	// Bounced beats all, and carries the reason.
	require.NoError(t, svc.ApplyEmailEvent(ctx, inv.ID, "", EmailStatusBounced, time.Now(), "HardBounce: gone"))
	row := listByID(t, svc, opTenant)[inv.ID]
	assert.Equal(t, EmailStatusBounced, *row.EmailStatus)
	require.NotNil(t, row.EmailStatusDetail)
	assert.Equal(t, "HardBounce: gone", *row.EmailStatusDetail)

	// Unknown invitations are swallowed — the webhook must 200 so Postmark
	// stops retrying events that can never apply.
	require.NoError(t, svc.ApplyEmailEvent(ctx, "", "pm-unknown", EmailStatusDelivered, time.Now(), ""))
	require.NoError(t, svc.ApplyEmailEvent(ctx, "", "", EmailStatusDelivered, time.Now(), ""))
}

// storedTokenHash reads what the database actually holds for the row — the
// one read that must never appear in any wire response.
func storedTokenHash(t *testing.T, svc *InvitationService, invitationID string) string {
	t.Helper()
	var hash string
	require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
		`SELECT token_hash FROM invitations WHERE id = $1`, invitationID).Scan(&hash))
	return hash
}

func listByID(t *testing.T, svc *InvitationService, tenantID string) map[string]models.Invitation {
	t.Helper()
	rows, err := svc.List(context.Background(), tenantID)
	require.NoError(t, err)
	out := map[string]models.Invitation{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func removeMembership(t *testing.T, svc *InvitationService, tenantID, wallet string) {
	t.Helper()
	_, err := svc.pdb.DBS().Writer.Exec(
		`DELETE FROM memberships WHERE tenant_id = $1 AND lower(wallet) = lower($2)`, tenantID, wallet)
	require.NoError(t, err)
}
