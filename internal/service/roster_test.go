package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Namespaced to this suite. The unique index on tenant_credentials is over
	// lower(dimo_client_id), so a generic 0xaaaa… collides case-insensitively
	// with another suite's fixture and the insert fails on a constraint that
	// has nothing to do with what is under test.
	licA = "0xf057e4000000000000000000000000000000ea01"
	licB = "0xf057e4000000000000000000000000000000eb02"

	ownerOld = "0xDA13fE288658C594Eac74d41ce9752474d4AD146"
	ownerNew = "0x97B8bA44C66d2C893925dE41BbDF0eE9b9640E7a"
)

// rosterFixture seeds two tenants each holding a developer licence, and clears
// the roster tables. Only the fixture's own rows are touched.
func rosterFixture(t *testing.T, store *db.Store) {
	t.Helper()
	seed(t, store)
	w := store.DBS().Writer

	_, err := w.Exec(`DELETE FROM vehicle_owner_changes`)
	require.NoError(t, err)
	_, err = w.Exec(`DELETE FROM vehicles`)
	require.NoError(t, err)

	// Clear any prior run's holder of these licence ids before re-inserting:
	// ON CONFLICT (tenant_id) resolves the primary key, not the unique index
	// over lower(dimo_client_id), so a leftover row on another tenant would
	// fail the insert.
	_, err = w.Exec(`UPDATE tenant_credentials SET dimo_client_id = NULL
		WHERE lower(dimo_client_id) = ANY($1)`, "{"+licA+","+licB+"}")
	require.NoError(t, err)

	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id) VALUES ($1,$2),($3,$4)
		ON CONFLICT (tenant_id) DO UPDATE SET dimo_client_id = EXCLUDED.dimo_client_id`,
		opTenant, licA, custTenant, licB)
	require.NoError(t, err)
}

func rosterSvc(t *testing.T, id *fakeIdentity) (*RosterService, *db.Store, context.Context) {
	t.Helper()
	store := testStore(t)
	rosterFixture(t, store)
	l := zerolog.Nop()
	return NewRosterService(&l, store, id), store, context.Background()
}

func rv(tokenID int64, owner, make_, model string) gateway.RosterVehicle {
	return gateway.RosterVehicle{TokenID: tokenID, Owner: owner, Make: make_, Model: model, Year: 2024}
}

func rosterOwner(t *testing.T, store *db.Store, tokenID int64) string {
	t.Helper()
	var owner string
	err := store.DBS().Reader.QueryRow(
		`SELECT COALESCE(owner,'') FROM vehicles WHERE vehicle_token_id=$1`, tokenID).Scan(&owner)
	require.NoError(t, err)
	return owner
}

// The population is the union over every licence, which is the whole reason
// this table can hold the roster and kaufmann could not: a vehicle reachable
// only through a self-serve tenant's own licence still lands here.
func TestReconcileSweepsEveryLicence(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(1, ownerNew, "Maxus", "T60")},
		licB: {rv(2, ownerNew, "Ford", "Ranger")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	// Counted, not asserted exactly: the sweep covers every credential in the
	// database, including other suites' fixtures, which is the production
	// behaviour under test. Pinning the number would make this test a
	// tripwire for unrelated fixtures rather than for the sweep.
	assert.GreaterOrEqual(t, rep.LicensesSwept, 2)
	assert.Equal(t, 2, rep.VehiclesSeen)
	assert.Equal(t, 2, rep.Inserted)
	assert.True(t, rep.FirstRun)

	// The property that matters: a vehicle reachable only through the second
	// licence is in the roster, which is what a single-source roster misses.
	assert.Equal(t, ownerNew, rosterOwner(t, store, 1))
	assert.Equal(t, ownerNew, rosterOwner(t, store, 2))
}

// A vehicle privileged to two licences is one roster row, not two, and is
// counted once.
func TestReconcileCollapsesDuplicateAcrossLicences(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(7, ownerNew, "Maxus", "T60")},
		licB: {rv(7, ownerNew, "Maxus", "T60")},
	}}
	svc, _, ctx := rosterSvc(t, id)

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.VehiclesSeen)
	assert.Equal(t, 1, rep.Inserted)
}

// THE CASE THE PLAN EXISTS FOR. A stored owner that the chain contradicts is
// corrected AND reported — the three Maxus T60s had been wrong since a transfer
// with nothing that would ever have said so.
func TestReconcileCorrectsAndReportsOwnerContradiction(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(192379, ownerOld, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)
	require.Equal(t, ownerOld, rosterOwner(t, store, 192379))

	// The chain now says someone else owns it.
	id.privileged[licA] = []gateway.RosterVehicle{rv(192379, ownerNew, "Maxus", "T60")}

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	require.Len(t, rep.OwnerChanges, 1, "the correction is reported, not just applied")
	assert.Equal(t, int64(192379), rep.OwnerChanges[0].TokenID)
	assert.Equal(t, ownerOld, rep.OwnerChanges[0].Previous)
	assert.Equal(t, ownerNew, rep.OwnerChanges[0].New)
	assert.Equal(t, ownerNew, rosterOwner(t, store, 192379), "and the roster follows the chain")

	var changes int
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT COUNT(*) FROM vehicle_owner_changes WHERE vehicle_token_id=$1`, 192379).Scan(&changes))
	assert.Equal(t, 2, changes, "first observation plus the transfer")
}

// A steady state must be quiet. If an unchanged owner reported a change every
// run, the log would be noise and a real transfer would hide in it.
func TestReconcileUnchangedOwnerIsNotAChange(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(5, ownerNew, "Ford", "Ranger")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)
	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	assert.Empty(t, rep.OwnerChanges)
	assert.False(t, rep.FirstRun)

	var changes int
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT COUNT(*) FROM vehicle_owner_changes WHERE vehicle_token_id=$1`, 5).Scan(&changes))
	assert.Equal(t, 1, changes, "only the first observation")
}

// -dry-run must compute the whole report and change nothing, so the first
// production run can be read before it acts.
func TestReconcileDryRunWritesNothing(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(11, ownerNew, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	rep, err := svc.Reconcile(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Inserted, "the report is complete")

	var n int
	require.NoError(t, store.DBS().Reader.QueryRow(`SELECT COUNT(*) FROM vehicles`).Scan(&n))
	assert.Zero(t, n, "and nothing was written")
}

// A licence that fails must not be laundered into "these vehicles are gone".
// Marking them unseen would record an identity-api outage as a fleet change.
func TestReconcilePartialSweepDoesNotMarkUnseen(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(21, ownerNew, "Ford", "Ranger")},
		licB: {rv(22, ownerNew, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	// Now every licence call fails.
	id.privilegedErr = errors.New("identity-api down")
	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	require.NotEmpty(t, rep.LicensesFailed)
	assert.Zero(t, rep.MarkedUnseen, "an outage is not a fleet change")

	var unseen int
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT COUNT(*) FROM vehicles WHERE unseen_since IS NOT NULL`).Scan(&unseen))
	assert.Zero(t, unseen)
}

// A clean sweep that no longer returns a vehicle stamps it rather than deleting
// it: losing sight of a vehicle is usually a revoked SACD, not a vehicle that
// stopped existing, and the row is the only record we ever knew it.
func TestReconcileMarksUnseenOnCleanSweep(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(31, ownerNew, "Ford", "Ranger"), rv(32, ownerNew, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	id.privileged[licA] = []gateway.RosterVehicle{rv(31, ownerNew, "Ford", "Ranger")}
	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	assert.Equal(t, 1, rep.MarkedUnseen)

	var n int
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT COUNT(*) FROM vehicles WHERE vehicle_token_id=$1`, 32).Scan(&n))
	assert.Equal(t, 1, n, "stamped, not deleted")

	// Reappearing clears the stamp — otherwise the column records the first
	// loss forever.
	id.privileged[licA] = []gateway.RosterVehicle{rv(31, ownerNew, "Ford", "Ranger"), rv(32, ownerNew, "Maxus", "T60")}
	_, err = svc.Reconcile(ctx, false)
	require.NoError(t, err)

	var unseen *time.Time
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT unseen_since FROM vehicles WHERE vehicle_token_id=$1`, 32).Scan(&unseen))
	assert.Nil(t, unseen)
}

// Plate is kaufmann's to write from the Chilean registry, and identity-api does
// not serve it. A reconcile that wrote the struct wholesale would blank a known
// plate every night — the roster being authoritative for a field it never
// reads is this plan's own failure mode, pointed the other way.
func TestReconcileDoesNotClearPlateOrVIN(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(41, ownerNew, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	_, err = store.DBS().Writer.Exec(
		`UPDATE vehicles SET license_plate='ABCD12', vin='WVWZZZ' WHERE vehicle_token_id=$1`, 41)
	require.NoError(t, err)

	_, err = svc.Reconcile(ctx, false)
	require.NoError(t, err)

	var plate, vin string
	require.NoError(t, store.DBS().Reader.QueryRow(
		`SELECT COALESCE(license_plate,''), COALESCE(vin,'') FROM vehicles WHERE vehicle_token_id=$1`,
		41).Scan(&plate, &vin))
	assert.Equal(t, "ABCD12", plate)
	assert.Equal(t, "WVWZZZ", vin)
}

// An empty owner from identity-api must not blank a known one. The roster's
// value is that this column is trustworthy; overwriting a good address with
// nothing because one read came back thin would make it exactly as reliable as
// the copy it replaces.
func TestReconcileEmptyOwnerDoesNotBlank(t *testing.T) {
	id := &fakeIdentity{privileged: map[string][]gateway.RosterVehicle{
		licA: {rv(51, ownerNew, "Maxus", "T60")},
	}}
	svc, store, ctx := rosterSvc(t, id)

	_, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	id.privileged[licA] = []gateway.RosterVehicle{rv(51, "", "Maxus", "T60")}
	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	assert.Empty(t, rep.OwnerChanges, "a missing read is not a transfer")
	assert.Equal(t, ownerNew, rosterOwner(t, store, 51))
}

// An entitled vehicle whose SACD is shared with no licence we hold is invisible
// to the sweep — but the entitlement is OUR record, so the token id is known
// and the roster must hold it anyway. Once readers cut over in step 4, an
// entitled vehicle missing from the roster is the empty-fleet incident again,
// one layer down.
func TestReconcileFillsEntitledVehicleTheSweepCannotSee(t *testing.T) {
	id := &fakeIdentity{
		privileged: map[string][]gateway.RosterVehicle{
			licA: {rv(61, ownerNew, "Ford", "Ranger")},
		},
		details: map[int64]gateway.RosterVehicle{
			62: rv(62, ownerNew, "Maxus", "T60"),
		},
	}
	svc, store, ctx := rosterSvc(t, id)

	_, err := store.DBS().Writer.Exec(
		`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source)
		 VALUES ($1, 62, 'direct') ON CONFLICT DO NOTHING`, custTenant)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(
			`DELETE FROM vehicle_entitlements WHERE tenant_id=$1 AND vehicle_token_id=62`, custTenant)
	})

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)

	assert.Equal(t, 1, rep.EntitledFilled)
	assert.Equal(t, 2, rep.VehiclesSeen, "the swept one plus the entitled one")
	assert.Equal(t, ownerNew, rosterOwner(t, store, 62))
}

// An entitlement pointing at a token identity-api does not know is a data
// problem to report, not a reason to abandon the whole reconcile.
func TestReconcileUnresolvableEntitledVehicleIsNotFatal(t *testing.T) {
	id := &fakeIdentity{
		privileged: map[string][]gateway.RosterVehicle{
			licA: {rv(71, ownerNew, "Ford", "Ranger")},
		},
		details: map[int64]gateway.RosterVehicle{}, // 72 is unknown to identity-api
	}
	svc, store, ctx := rosterSvc(t, id)

	_, err := store.DBS().Writer.Exec(
		`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source)
		 VALUES ($1, 72, 'direct') ON CONFLICT DO NOTHING`, custTenant)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(
			`DELETE FROM vehicle_entitlements WHERE tenant_id=$1 AND vehicle_token_id=72`, custTenant)
	})

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err, "one unresolvable entitlement must not fail the run")
	assert.Zero(t, rep.EntitledFilled)
	assert.Equal(t, 1, rep.VehiclesSeen)
}

// A revoked entitlement is not a gap to fill. Filling it would put a vehicle
// the operator took away back into the roster every night.
func TestReconcileIgnoresRevokedEntitlements(t *testing.T) {
	id := &fakeIdentity{
		privileged: map[string][]gateway.RosterVehicle{licA: {rv(81, ownerNew, "Ford", "Ranger")}},
		details:    map[int64]gateway.RosterVehicle{82: rv(82, ownerNew, "Maxus", "T60")},
	}
	svc, store, ctx := rosterSvc(t, id)

	_, err := store.DBS().Writer.Exec(
		`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source, revoked_at)
		 VALUES ($1, 82, 'direct', NOW()) ON CONFLICT DO NOTHING`, custTenant)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(
			`DELETE FROM vehicle_entitlements WHERE tenant_id=$1 AND vehicle_token_id=82`, custTenant)
	})

	rep, err := svc.Reconcile(ctx, false)
	require.NoError(t, err)
	assert.Zero(t, rep.EntitledFilled)
}
