package controllers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gofiber/fiber/v2"
	_ "github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared-ops endpoint end to end: real controller, real TenantService and
// ShareAuthorizer against the local database, faked upstream gateways and a
// recording queue. This is the surface plan 06 step 3 exists for, and its
// status-code contract is what kaufmann's step-4 poll loop will be written
// against — so the mapping is pinned here rather than left to be inferred
// from the worker tests.
//
// Fixture ids are distinct from every other package's: the test databases are
// shared, and the unique index on lower(dimo_client_id) makes collisions
// test-order-dependent.
const (
	opsTenant       = "eeeeeeee-0000-0000-0000-000000000001"
	opsChildTenant  = "eeeeeeee-0000-0000-0000-000000000002"
	opsForeign      = "eeeeeeee-0000-0000-0000-00000000000f"
	opsClientID     = "0xEEEE000000000000000000000000000000000001"
	opsOwnerWallet  = "0xEEEE00000000000000000000000000000000000A"
	opsTargetWallet = "0xEEEE00000000000000000000000000000000000B"
	opsEncKey       = "shared-ops-test-enc-key"
	opsTokenID      = int64(42)
)

type opsIdentity struct{ owners map[int64]string }

func (f *opsIdentity) RedirectURIForClientID(string) (string, error) { return "https://x.example", nil }
func (f *opsIdentity) VehicleOwner(tokenID int64) (string, error) {
	owner, ok := f.owners[tokenID]
	if !ok {
		return "", gateway.ErrVehicleNotFound
	}
	return owner, nil
}
func (f *opsIdentity) PrivilegedVehicles(string) ([]gateway.RosterVehicle, error) { return nil, nil }
func (f *opsIdentity) VehicleDetail(int64) (*gateway.RosterVehicle, error) {
	return nil, gateway.ErrVehicleNotFound
}

type opsAccounts struct{ signer string }

func (f *opsAccounts) GetAccountByEmail(string, string) (*gateway.Account, error) {
	return nil, gateway.ErrAccountNotFound
}
func (f *opsAccounts) CreateAccount(string, string, string) (*gateway.Account, error) {
	return nil, gateway.ErrAccountNotFound
}
func (f *opsAccounts) GetAccountByWallet(wallet, _ string) (*gateway.Account, error) {
	return &gateway.Account{WalletAddress: wallet, ProvidedSignerAddress: f.signer}, nil
}

type opsCreds struct{ effective *service.EffectiveCredential }

func (f *opsCreds) DeveloperJWT(context.Context, string) (*models.MintedToken, error) {
	return &models.MintedToken{Token: "jwt"}, nil
}
func (f *opsCreds) Effective(context.Context, string) (*service.EffectiveCredential, error) {
	return f.effective, nil
}

// opsQueue records enqueues and serves canned statuses, so the tests can
// assert exactly what would have been queued without River.
type opsQueue struct {
	enqueued   []sharing.SharedOpArgs
	enqueueErr error
	status     *models.ShareStatus
	statusErr  error
}

func (q *opsQueue) Enqueue(context.Context, sharing.ShareArgs) (int64, error) {
	return 0, fmt.Errorf("unexpected share enqueue in a shared-ops test")
}
func (q *opsQueue) EnqueueRevoke(context.Context, sharing.RevokeArgs) (int64, error) {
	return 0, fmt.Errorf("unexpected revoke enqueue in a shared-ops test")
}
func (q *opsQueue) Status(context.Context, string, int64) (*models.ShareStatus, error) {
	return nil, fmt.Errorf("unexpected share status read in a shared-ops test")
}
func (q *opsQueue) EnqueueSharedOp(_ context.Context, args sharing.SharedOpArgs) (int64, error) {
	if q.enqueueErr != nil {
		return 0, q.enqueueErr
	}
	q.enqueued = append(q.enqueued, args)
	return 1234, nil
}
func (q *opsQueue) SharedOpStatus(context.Context, string, int64) (*models.ShareStatus, error) {
	return q.status, q.statusErr
}

// opsFixture stands the whole stack up: tenant rows (an implicit operator
// holding the credential and signer, and an explicit managed child with no
// rows of its own), a real signer key encrypted under the test master key, and
// a fiber app routing to the real controller.
func opsFixture(t *testing.T, queue *opsQueue, clientID string) *fiber.App {
	t.Helper()

	settings := db.Settings{
		User: "dimo", Password: "dimo", Host: "localhost", Port: "5432",
		Name: "fleet_tenancy_api", SSLMode: "disable",
		MaxOpenConnections: 5, MaxIdleConnections: 2,
	}
	if v := os.Getenv("FLEET_TENANCY_TEST_HOST"); v != "" {
		settings.Host = v
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		settings.Host, settings.Port, settings.User, settings.Password, settings.Name, settings.SSLMode)
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := probe.Ping(); err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}

	store := db.NewDbConnectionFromSettings(context.Background(), &settings, true)
	store.WaitForDB(zerolog.Nop())
	w := store.DBS().Writer

	cleanup := func() {
		// The signer gate remembers what accounts-api said, so a negative left
		// by an earlier run would deny this one before the fake is consulted.
		_, _ = w.Exec(`DELETE FROM shared_accounts WHERE lower(wallet) = lower($1)`, opsOwnerWallet)
		_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`,
			"{"+opsTenant+","+opsChildTenant+"}")
		_, _ = w.Exec(`DELETE FROM tenants WHERE id = ANY($1)`,
			"{"+opsChildTenant+","+opsTenant+"}")
	}
	cleanup()
	t.Cleanup(cleanup)

	_, err = w.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode) VALUES
		($1,'OpsOp','operator','implicit'), ($2,'OpsChild','customer','explicit')`,
		opsTenant, opsChildTenant)
	require.NoError(t, err)
	_, err = w.Exec(`UPDATE tenants SET parent_tenant_id=$1 WHERE id=$2`, opsTenant, opsChildTenant)
	require.NoError(t, err)

	// A real key pair: the authorizer decrypts and parses it, so a placeholder
	// string would fail in signerKey rather than exercising the happy path.
	signerPK, err := crypto.GenerateKey()
	require.NoError(t, err)
	signerAddr := crypto.PubkeyToAddress(signerPK.PublicKey).Hex()
	keyEnc, err := service.EncryptSecret(opsEncKey, common.Bytes2Hex(crypto.FromECDSA(signerPK)))
	require.NoError(t, err)
	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id, signer_address, signer_key_enc)
		VALUES ($1,$2,$3,$4)`, opsTenant, opsClientID, signerAddr, keyEnc)
	require.NoError(t, err)

	logger := zerolog.Nop()
	cfg := &config.Settings{TenantSecretEncKey: opsEncKey}
	creds := &opsCreds{effective: &service.EffectiveCredential{
		TenantID: opsTenant, ClientID: clientID, SignerAddress: signerAddr,
	}}
	signerSvc := service.NewSharedSignerService(&logger, &opsAccounts{signer: signerAddr}, creds, service.NewSharedAccountStore(&store))
	shares := service.NewShareAuthorizer(&logger, &store,
		&opsIdentity{owners: map[int64]string{opsTokenID: opsOwnerWallet}}, signerSvc, creds, cfg)
	tenants := service.NewTenantService(&logger, &store)
	ctrl := NewSharingController(&logger, signerSvc, nil, shares, queue, tenants,
		func(*fiber.Ctx) *models.CallerTenant { return &models.CallerTenant{TenantID: opsTenant} })

	app := fiber.New()
	app.Post("/v1/tenants/:tenantId/vehicles/:tokenId/shared-ops", ctrl.SharedOperation)
	app.Get("/v1/tenants/:tenantId/vehicles/:tokenId/shared-ops/status", ctrl.SharedOperationStatus)
	return app
}

func postSharedOp(t *testing.T, app *fiber.App, tenantID string, tokenID interface{}, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/tenants/%s/vehicles/%v/shared-ops", tenantID, tokenID),
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	return resp
}

func TestSharedOperationEndpoint(t *testing.T) {
	t.Run("a valid transfer is accepted and queued as typed args", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, opsTokenID,
			`{"op":"transfer_vehicle","targetWallet":"`+opsTargetWallet+`"}`)

		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		var out models.SharedOpResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		assert.Equal(t, int64(1234), out.JobID)

		require.Len(t, queue.enqueued, 1)
		got := queue.enqueued[0]
		assert.Equal(t, sharing.OpTransferVehicle, got.Op)
		assert.Equal(t, opsTenant, got.TenantID)
		assert.Equal(t, opsTokenID, got.TokenID)
		assert.Equal(t, opsTargetWallet, got.TargetWallet)
	})

	t.Run("burn_synthetic carries the caller-supplied device id", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, opsTokenID, `{"op":"burn_synthetic","syntheticTokenId":77}`)

		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		require.Len(t, queue.enqueued, 1)
		assert.Equal(t, int64(77), queue.enqueued[0].SyntheticTokenID)
	})

	// THE SECURITY BOUNDARY. There is no op value that carries calldata, and
	// anything outside the enum — or wearing another op's fields — dies as a
	// 400 before any authorization work, let alone the queue.
	t.Run("the enum is closed", func(t *testing.T) {
		for name, body := range map[string]string{
			"an unknown op":               `{"op":"execute","calldata":"0xdeadbeef"}`,
			"a missing op":                `{}`,
			"a transfer with no target":   `{"op":"transfer_vehicle"}`,
			"a burn with another's field": `{"op":"burn_vehicle","targetWallet":"` + opsTargetWallet + `"}`,
			"a synthetic burn with no id": `{"op":"burn_synthetic"}`,
		} {
			queue := &opsQueue{}
			app := opsFixture(t, queue, opsClientID)
			resp := postSharedOp(t, app, opsTenant, opsTokenID, body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
			assert.Empty(t, queue.enqueued, "%s must never reach the queue", name)
		}
	})

	t.Run("transferring to the current owner is refused synchronously", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, opsTokenID,
			`{"op":"transfer_vehicle","targetWallet":"`+opsOwnerWallet+`"}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, queue.enqueued)
	})

	t.Run("grant_sacd with a usable client id is accepted", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, opsTokenID, `{"op":"grant_sacd"}`)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	})

	t.Run("grant_sacd without one is a 409, found before the job exists", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, "")
		resp := postSharedOp(t, app, opsTenant, opsTokenID, `{"op":"grant_sacd"}`)
		assert.Equal(t, http.StatusConflict, resp.StatusCode)
		assert.Empty(t, queue.enqueued)
	})

	t.Run("a caller outside the tenant's scope is refused", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsForeign, opsTokenID,
			`{"op":"transfer_vehicle","targetWallet":"`+opsTargetWallet+`"}`)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Empty(t, queue.enqueued)
	})

	// The scope rule's child branch: the operator reaches its managed customer
	// — but the customer is explicit-mode with no entitlement row for this
	// vehicle, so the authorization chain refuses. Scope and entitlement are
	// different fences and both must hold.
	t.Run("an entitlement gap on a managed child is a 403", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsChildTenant, opsTokenID,
			`{"op":"transfer_vehicle","targetWallet":"`+opsTargetWallet+`"}`)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(body), "fleet", "the refusal names the entitlement, not the scope")
	})

	t.Run("an unknown vehicle is a 404", func(t *testing.T) {
		queue := &opsQueue{}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, 9999,
			`{"op":"transfer_vehicle","targetWallet":"`+opsTargetWallet+`"}`)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("an unconfigured queue is a 503, not a 500", func(t *testing.T) {
		queue := &opsQueue{enqueueErr: sharing.ErrQueueUnavailable}
		app := opsFixture(t, queue, opsClientID)
		resp := postSharedOp(t, app, opsTenant, opsTokenID,
			`{"op":"transfer_vehicle","targetWallet":"`+opsTargetWallet+`"}`)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
}

func TestSharedOperationStatusEndpoint(t *testing.T) {
	get := func(t *testing.T, app *fiber.App, tenantID, query string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v1/tenants/%s/vehicles/42/shared-ops/status%s", tenantID, query), nil)
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		return resp
	}

	t.Run("mirrors the ShareStatus shape", func(t *testing.T) {
		queue := &opsQueue{status: &models.ShareStatus{
			JobID: 7, State: "completed", IsSuccessful: true, Errors: []string{},
		}}
		app := opsFixture(t, queue, opsClientID)
		resp := get(t, app, opsTenant, "?jobId=7")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out models.ShareStatus
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		assert.Equal(t, int64(7), out.JobID)
		assert.True(t, out.IsSuccessful)
	})

	t.Run("not-found stays not-found", func(t *testing.T) {
		queue := &opsQueue{statusErr: sharing.ErrJobNotFound}
		app := opsFixture(t, queue, opsClientID)
		resp := get(t, app, opsTenant, "?jobId=7")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("a missing jobId is a 400", func(t *testing.T) {
		app := opsFixture(t, &opsQueue{}, opsClientID)
		resp := get(t, app, opsTenant, "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("scope is checked here too", func(t *testing.T) {
		queue := &opsQueue{status: &models.ShareStatus{JobID: 7}}
		app := opsFixture(t, queue, opsClientID)
		resp := get(t, app, opsForeign, "?jobId=7")
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
