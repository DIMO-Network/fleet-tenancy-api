package service

import (
	"strings"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The metadata read is the join step 4 cuts fleet-lite over to: named tokens
// in, roster rows out, in token order.
func TestMetadataReturnsNamedRows(t *testing.T) {
	minted := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	v := rv(4101, ownerNew, "Maxus", "T60")
	v.MintedAt = &minted
	v.DefinitionID = "maxus_t60_2024"

	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {v, rv(4102, ownerNew, "Ford", "Ranger")},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4102, 4101})
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, int64(4101), got[0].VehicleTokenID, "ordered by token id, not by the caller's order")
	assert.Equal(t, ownerNew, got[0].Owner)
	assert.Equal(t, "Maxus", got[0].Make)
	assert.Equal(t, "T60", got[0].Model)
	assert.Equal(t, 2024, got[0].Year)
	assert.Equal(t, "maxus_t60_2024", got[0].DefinitionID)
	require.NotNil(t, got[0].MintedAt)
	assert.True(t, minted.Equal(*got[0].MintedAt))
	assert.False(t, got[0].ReconciledAt.IsZero(), "staleness must be visible as a timestamp")
	assert.Nil(t, got[0].UnseenSince)

	assert.Equal(t, int64(4102), got[1].VehicleTokenID)
}

// THE CASE STEP 4 MUST NOT GET WRONG. A token the roster has never seen is not
// an error and not an exclusion — it comes back absent, and the caller renders
// the vehicle with what it has. A vehicle entitled minutes ago, before any
// reconcile has run, is exactly this case, and dropping it would be the
// empty-fleet incident again one layer down.
func TestMetadataMissingTokenIsAbsentNotAnError(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(4201, ownerNew, "Ford", "Ranger")},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4201, 999_000_001})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(4201), got[0].VehicleTokenID)
}

// Every token unknown is the same case, not a different one: a freshly entitled
// customer before the first reconcile gets an empty join, never a failure.
func TestMetadataAllTokensMissing(t *testing.T) {
	svc, _, ctx := rosterSvc(t, &fakeIdentity{})

	got, err := svc.Metadata(ctx, []int64{999_000_002, 999_000_003})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// An empty request is a real state — a customer between entitlements — and must
// not reach the database or produce an error.
func TestMetadataEmptyRequest(t *testing.T) {
	svc, _, ctx := rosterSvc(t, &fakeIdentity{})

	got, err := svc.Metadata(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A vehicle no licence returns any more still answers, carrying the stamp that
// says when we lost sight of it. Hiding it would remove a vehicle from a fleet
// because of a revoked SACD, silently — the failure mode this plan is about.
func TestMetadataReportsUnseen(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(4301, ownerNew, "Ford", "Ranger")},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	id.privileged[licA] = nil
	_, err = svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4301})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].UnseenSince, "the row answers, and says since when it was unseen")
}

// Owner comes back EIP-55 whatever case the row holds, so a caller comparing it
// against its own stored address as a string cannot silently miss.
func TestMetadataChecksumsOwner(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(4401, strings.ToLower(ownerNew), "Ford", "Ranger")},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4401})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ownerNew, got[0].Owner)
}

// VIN and plate are kaufmann's to write and this table's to serve. A row with
// neither is normal — identity-api serves neither field — and must not be
// mistaken for a missing vehicle.
func TestMetadataServesVinAndPlateWhenPresent(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(4501, ownerNew, "Maxus", "T60"), rv(4502, ownerNew, "Ford", "Ranger")},
	}}
	svc, store, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	_, err = store.DBS().Writer.Exec(
		`UPDATE vehicles SET vin = $2, license_plate = $3 WHERE vehicle_token_id = $1`,
		4501, "LSGBL1234RA000001", "ABCD12")
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4501, 4502})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "LSGBL1234RA000001", got[0].VIN)
	assert.Equal(t, "ABCD12", got[0].LicensePlate)
	assert.Empty(t, got[1].VIN)
	assert.Empty(t, got[1].LicensePlate)
}

// Device token ids are what a caller renders a connection indicator from —
// fleet-lite's list guards on `tokenId > 0`. Without them in the roster, a
// cutover blanks that indicator for every vehicle, which is why step 4 added
// the columns before touching any reader.
func TestMetadataServesDeviceTokenIDs(t *testing.T) {
	synthetic := int64(55501)
	aftermarket := int64(55502)

	paired := rv(4601, ownerNew, "Maxus", "T60")
	paired.SyntheticDeviceTokenID = &synthetic
	paired.AftermarketDeviceTokenID = &aftermarket

	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {paired, rv(4602, ownerNew, "Ford", "Ranger")},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4601, 4602})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.NotNil(t, got[0].SyntheticDeviceTokenID)
	assert.Equal(t, synthetic, *got[0].SyntheticDeviceTokenID)
	require.NotNil(t, got[0].AftermarketDeviceTokenID)
	assert.Equal(t, aftermarket, *got[0].AftermarketDeviceTokenID)

	assert.Nil(t, got[1].SyntheticDeviceTokenID, "an unpaired vehicle says so")
	assert.Nil(t, got[1].AftermarketDeviceTokenID)
}

// THE ONE THAT MAKES DEVICE IDS DIFFERENT FROM VIN AND PLATE. Unpairing is a
// real event, and the roster must show it. VIN and plate are filled forward
// because identity-api does not serve them; these it does, so a null from the
// chain is an answer and must overwrite.
func TestReconcileClearsUnpairedDevice(t *testing.T) {
	synthetic := int64(55503)
	paired := rv(4701, ownerNew, "Maxus", "T60")
	paired.SyntheticDeviceTokenID = &synthetic

	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {paired},
	}}
	svc, _, ctx := rosterSvc(t, id)
	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err := svc.Metadata(ctx, []int64{4701})
	require.NoError(t, err)
	require.NotNil(t, got[0].SyntheticDeviceTokenID, "paired to begin with")

	// The device is unpaired on chain.
	id.privileged[licA] = []gateway.RosterVehicle{rv(4701, ownerNew, "Maxus", "T60")}
	_, err = svc.Reconcile(ctx, false)
	require.NoError(t, err)

	got, err = svc.Metadata(ctx, []int64{4701})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].SyntheticDeviceTokenID,
		"the roster follows the chain; a filled-forward id would read as connected forever")
}
