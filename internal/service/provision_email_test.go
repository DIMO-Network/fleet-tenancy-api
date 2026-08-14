package service

import (
	"context"
	"errors"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNotifier records the access-granted sends provisioning attempts.
type fakeNotifier struct {
	enabled bool
	err     error

	to         string
	tenantName string
	calls      int
}

func (f *fakeNotifier) Enabled() bool { return f.enabled }
func (f *fakeNotifier) SendAccessGranted(_ context.Context, toEmail, tenantName string) error {
	f.calls++
	f.to = toEmail
	f.tenantName = tenantName
	return f.err
}

// The email is a courtesy on top of the grant, and the response must be honest
// about it in both directions: EmailSent true only when a send actually
// happened, and a failed send must not fail a provision whose membership is
// already written and visible — that would report a grant that happened as one
// that did not.
func TestProvisionAccessEmail(t *testing.T) {
	token := &models.MintedToken{Token: "jwt-abc", ClientID: "0xCCCC000000000000000000000000000000000001"}
	cred := &EffectiveCredential{TenantID: opTenant, ClientID: token.ClientID,
		SignerAddress: "0x9999000000000000000000000000000000000009"}
	req := func() *models.ProvisionRequest {
		return &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email:             "person@example.com",
			Role:              "member",
			Permissions:       []string{models.CapReports},
			ScopeGroupIDs:     scope(`[]`),
			GrantedByTenantID: opTenant,
		}}
	}

	t.Run("a successful send is reported, with the tenant's name in it", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{WalletAddress: provisionedWallet}}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)
		notifier := &fakeNotifier{enabled: true}
		svc.UseAccessEmail(notifier)

		res, err := svc.Provision(ctx, custTenant, req())
		require.NoError(t, err)
		assert.True(t, res.EmailSent)
		assert.Equal(t, 1, notifier.calls)
		assert.Equal(t, "person@example.com", notifier.to)
		assert.NotEmpty(t, notifier.tenantName, "the email names the tenant, from the tenants table")
		assert.NotEqual(t, "your fleet", notifier.tenantName,
			"a seeded tenant has a real name; the fallback means the lookup silently failed")
	})

	t.Run("a failed send does not fail the provision", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{WalletAddress: provisionedWallet}}
		svc, authz, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)
		svc.UseAccessEmail(&fakeNotifier{enabled: true, err: errors.New("postmark 500")})

		res, err := svc.Provision(ctx, custTenant, req())
		require.NoError(t, err, "the membership is written; the email is a courtesy")
		assert.False(t, res.EmailSent, "and the response says so, rather than counting it sent")

		got, aerr := authz.Authorize(ctx, custTenant, provisionedWallet)
		require.NoError(t, aerr)
		assert.True(t, got.Member, "the grant stands despite the failed email")
	})

	t.Run("unconfigured email is EmailSent false, not an attempt", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{WalletAddress: provisionedWallet}}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)
		notifier := &fakeNotifier{enabled: false}
		svc.UseAccessEmail(notifier)

		res, err := svc.Provision(ctx, custTenant, req())
		require.NoError(t, err)
		assert.False(t, res.EmailSent)
		assert.Zero(t, notifier.calls, "a disabled notifier must not be called at all")
	})

	t.Run("no notifier wired at all is the same", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{WalletAddress: provisionedWallet}}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)

		res, err := svc.Provision(ctx, custTenant, req())
		require.NoError(t, err)
		assert.False(t, res.EmailSent)
	})
}

// fakeTemplatedSender stands in for the Postmark client under AccessEmailService.
type fakeTemplatedSender struct {
	enabled bool
	from    string
	to      string
	alias   string
	model   any
}

func (f *fakeTemplatedSender) Enabled() bool { return f.enabled }
func (f *fakeTemplatedSender) SendTemplated(from, to, alias string, model any) error {
	f.from, f.to, f.alias, f.model = from, to, alias, model
	return nil
}

func TestAccessEmailService(t *testing.T) {
	l := zerolog.Nop()

	t.Run("sends with the configured from, alias and login link", func(t *testing.T) {
		sender := &fakeTemplatedSender{enabled: true}
		svc := &AccessEmailService{logger: &l, sender: sender,
			from: "no-reply@dimo.co", alias: "fleet-access-granted", loginURL: "https://fleets.dimo.co"}

		require.True(t, svc.Enabled())
		require.NoError(t, svc.SendAccessGranted(context.Background(), "person@example.com", "Kaufmann"))
		assert.Equal(t, "no-reply@dimo.co", sender.from)
		assert.Equal(t, "person@example.com", sender.to)
		assert.Equal(t, "fleet-access-granted", sender.alias)
		assert.Equal(t, accessGrantedModel{TenantName: "Kaufmann", LoginURL: "https://fleets.dimo.co"}, sender.model)
	})

	t.Run("a token without a from address is not enabled", func(t *testing.T) {
		// Postmark would reject the send anyway; better to report the feature
		// off than to attempt sends that always fail.
		svc := &AccessEmailService{logger: &l, sender: &fakeTemplatedSender{enabled: true}}
		assert.False(t, svc.Enabled())
	})
}
