package sharing

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	sharedb "github.com/DIMO-Network/shared/pkg/db"
	_ "github.com/lib/pq"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture tenants for the queue tests, named so a leaked row is identifiable.
const (
	queueTenantA = "cccccccc-0000-0000-0000-00000000000a"
	queueTenantB = "cccccccc-0000-0000-0000-00000000000b"
)

// queueFixture builds a real Queue against the local Postgres the repo's other
// DB tests use (the same convention as service.testStore), with both workers
// registered and the client never started — Insert and JobGet work without
// Start, and not starting means nothing tries to reach a bundler.
func queueFixture(t *testing.T) *Queue {
	t.Helper()

	settings := fullySharingConfigured(t)
	settings.DB = sharedb.Settings{
		User: "dimo", Password: "dimo", Host: "localhost", Port: "5432",
		Name: "fleet_tenancy_api", SSLMode: "disable",
		MaxOpenConnections: 5, MaxIdleConnections: 2,
	}
	if v := os.Getenv("FLEET_TENANCY_TEST_HOST"); v != "" {
		settings.DB.Host = v
	}

	// Probe with a plain connection first so an absent database skips rather
	// than failing — CI without Postgres still passes, a laptop with it runs.
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		settings.DB.Host, settings.DB.Port, settings.DB.User, settings.DB.Password,
		settings.DB.Name, settings.DB.SSLMode)
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}
	if err := probe.Ping(); err != nil {
		_ = probe.Close()
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}

	logger := zerolog.Nop()
	workers := river.NewWorkers()
	require.NoError(t, river.AddWorkerSafely(workers,
		NewShareWorker(&logger, settings, &stubAuthorizer{}, &stubFleet{}, nil)))
	require.NoError(t, river.AddWorkerSafely(workers,
		NewRevokeWorker(&logger, settings, &stubAuthorizer{}, &stubFleet{}, nil)))
	require.NoError(t, river.AddWorkerSafely(workers,
		NewSharedOpWorker(&logger, settings, &stubOpAuthorizer{}, &stubSignerGate{}, &opFleet{})))

	q, err := NewQueue(context.Background(), &logger, settings, workers)
	require.NoError(t, err)
	require.NotNil(t, q, "sharing is configured and workers exist, so a queue must build")

	t.Cleanup(func() {
		// Delete only this fixture's jobs — the shared local database may hold
		// anything. The tenant ids above exist for this WHERE clause.
		_, _ = probe.Exec(`DELETE FROM fleet_tenancy_api.river_job
			WHERE args->>'tenantId' IN ($1, $2)`, queueTenantA, queueTenantB)
		_ = probe.Close()
		q.pool.Close()
	})
	return q
}

// The two status readers share one queue, one id sequence and one shape — and
// must not answer for each other's jobs or for other tenants' jobs. This is
// the property the endpoint's security note hangs on (sequential ids are cheap
// to walk), so it is tested against a real River queue rather than inferred.
func TestQueue_SharedOpStatusScoping(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()

	opJobID, err := q.EnqueueSharedOp(ctx, SharedOpArgs{
		TenantID: queueTenantA, TokenID: 42, Op: OpBurnVehicle,
	})
	require.NoError(t, err)
	shareJobID, err := q.Enqueue(ctx, ShareArgs{
		TenantID: queueTenantA, TokenID: 42,
		Grantee: testGrantee.Hex(),
	})
	require.NoError(t, err)

	t.Run("the owning tenant reads its job, in the ShareStatus shape", func(t *testing.T) {
		status, err := q.SharedOpStatus(ctx, queueTenantA, opJobID)
		require.NoError(t, err)
		assert.Equal(t, opJobID, status.JobID)
		assert.Equal(t, "available", status.State, "never started, so the job is still queued")
		assert.False(t, status.IsSuccessful)
		assert.Empty(t, status.Errors)
	})

	t.Run("another tenant gets not-found, not forbidden", func(t *testing.T) {
		_, err := q.SharedOpStatus(ctx, queueTenantB, opJobID)
		assert.ErrorIs(t, err, ErrJobNotFound,
			"confirming the job exists would tell a caller what other tenants have run")
	})

	t.Run("a share job id is not a shared-op job id", func(t *testing.T) {
		_, err := q.SharedOpStatus(ctx, queueTenantA, shareJobID)
		assert.ErrorIs(t, err, ErrJobNotFound,
			"same queue, same id sequence — the kind check is what keeps the two surfaces distinct")
	})

	t.Run("an id that was never issued is not-found", func(t *testing.T) {
		_, err := q.SharedOpStatus(ctx, queueTenantA, 1<<60)
		assert.ErrorIs(t, err, ErrJobNotFound)
	})

	t.Run("the share status endpoint still serves the share job", func(t *testing.T) {
		status, err := q.Status(ctx, queueTenantA, shareJobID)
		require.NoError(t, err)
		assert.Equal(t, shareJobID, status.JobID)
	})

	t.Run("and the kind check holds in the other direction too", func(t *testing.T) {
		_, err := q.Status(ctx, queueTenantA, opJobID)
		assert.ErrorIs(t, err, ErrJobNotFound,
			"a shared-op job's args decode into ShareArgs cleanly enough to leak without the kind check")
	})
}

// A nil queue — sharing unconfigured — answers with the named unavailability
// error for the new surface exactly as for the old one, so the controller can
// 503 rather than 500.
func TestNilQueue_SharedOpSurfaceIsUnavailable(t *testing.T) {
	var q *Queue
	_, err := q.EnqueueSharedOp(context.Background(), SharedOpArgs{})
	assert.ErrorIs(t, err, ErrQueueUnavailable)
	_, err = q.SharedOpStatus(context.Background(), "t1", 1)
	assert.ErrorIs(t, err, ErrQueueUnavailable)
	_, err = q.EnqueueRevoke(context.Background(), RevokeArgs{})
	assert.ErrorIs(t, err, ErrQueueUnavailable)
}

// THE DELIBERATE EXCEPTION to the kind check: Status answers for a revocation
// as well as a share, because the two are directions of one relationship and a
// caller should not learn a second protocol to poll the second one. What the
// kind check still keeps out is shared-operation jobs.
//
// Tested against a real River queue rather than inferred, because this is the
// property the endpoint's security note hangs on — job ids are sequential and
// cheap to walk.
func TestQueue_StatusAnswersForRevocationsButNotSharedOps(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()

	revokeJobID, err := q.EnqueueRevoke(ctx, RevokeArgs{
		TenantID: queueTenantA, TokenID: 42, Grantee: testGrantee.Hex(),
	})
	require.NoError(t, err)
	opJobID, err := q.EnqueueSharedOp(ctx, SharedOpArgs{
		TenantID: queueTenantA, TokenID: 42, Op: OpBurnVehicle,
	})
	require.NoError(t, err)

	t.Run("the owning tenant polls a revocation on the share status route", func(t *testing.T) {
		status, err := q.Status(ctx, queueTenantA, revokeJobID)
		require.NoError(t, err)
		assert.Equal(t, revokeJobID, status.JobID)
		assert.Equal(t, "available", status.State)
		assert.False(t, status.IsSuccessful)
	})

	t.Run("another tenant gets not-found, not forbidden", func(t *testing.T) {
		_, err := q.Status(ctx, queueTenantB, revokeJobID)
		assert.ErrorIs(t, err, ErrJobNotFound,
			"a revocation is scoped exactly as a share is")
	})

	t.Run("widening for revocations did not widen for shared ops", func(t *testing.T) {
		_, err := q.Status(ctx, queueTenantA, opJobID)
		assert.ErrorIs(t, err, ErrJobNotFound,
			"the kind check must still separate the two surfaces")
	})

	t.Run("the shared-op reader does not answer for a revocation either", func(t *testing.T) {
		_, err := q.SharedOpStatus(ctx, queueTenantA, revokeJobID)
		assert.ErrorIs(t, err, ErrJobNotFound)
	})
}
