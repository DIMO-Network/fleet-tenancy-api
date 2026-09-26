package sharing

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSequencer answers "is an earlier job for this vehicle unfinished?"
// without a database, and records whether it was asked.
func fakeSequencer(blocked bool, err error) (*vehicleSequencer, *[]string) {
	asked := []string{}
	return &vehicleSequencer{
		logger: zerolog.Nop(),
		pending: func(_ context.Context, _ int64, tokenID string) (bool, error) {
			asked = append(asked, tokenID)
			return blocked, err
		},
	}, &asked
}

func runThrough(t *testing.T, s *vehicleSequencer, args string) (ran bool, err error) {
	t.Helper()
	job := &rivertype.JobRow{ID: 7, Kind: "vehicle_share", EncodedArgs: []byte(args)}
	err = s.Work(context.Background(), job, func(context.Context) error {
		ran = true
		return nil
	})
	return ran, err
}

func assertSnoozed(t *testing.T, err error) {
	t.Helper()
	var snooze *rivertype.JobSnoozeError
	require.True(t, errors.As(err, &snooze), "want a snooze, got %v", err)
	assert.Equal(t, sequenceSnooze, snooze.Duration)
}

func TestVehicleSequencer_HoldsAJobBehindAnEarlierOne(t *testing.T) {
	s, asked := fakeSequencer(true, nil)
	ran, err := runThrough(t, s, `{"tenantId":"t","tokenId":42,"grantee":"0xabc"}`)
	assert.False(t, ran, "a job must not run while an earlier one for its vehicle is unfinished")
	assertSnoozed(t, err)
	assert.Equal(t, []string{"42"}, *asked)
}

func TestVehicleSequencer_RunsWhenNothingEarlierIsUnfinished(t *testing.T) {
	s, _ := fakeSequencer(false, nil)
	ran, err := runThrough(t, s, `{"tenantId":"t","tokenId":42}`)
	assert.True(t, ran)
	assert.NoError(t, err)
}

func TestVehicleSequencer_HoldsWhenItCannotCheck(t *testing.T) {
	s, _ := fakeSequencer(false, errors.New("connection refused"))
	ran, err := runThrough(t, s, `{"tenantId":"t","tokenId":42}`)
	assert.False(t, ran, "not knowing is not permission to run out of order")
	assertSnoozed(t, err)
}

func TestVehicleSequencer_PassesThroughJobsWithoutAVehicle(t *testing.T) {
	for _, args := range []string{`{"tenantId":"t"}`, `{"tokenId":"not-a-number"}`, `not json`} {
		s, asked := fakeSequencer(true, nil)
		ran, err := runThrough(t, s, args)
		assert.True(t, ran, args)
		assert.NoError(t, err, args)
		assert.Empty(t, *asked, "no vehicle, nothing to look up: %s", args)
	}
}

// The ordering property end to end against a real River table: an upgrade
// queued before a revoke for the same vehicle must finish first, however the
// workers happen to pick them up. Another vehicle is unaffected.
func TestVehicleSequencer_OrdersJobsPerVehicleAgainstRiver(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()

	upgradeID, err := q.Enqueue(ctx, ShareArgs{TenantID: queueTenantA, TokenID: 42, Grantee: testGrantee.Hex()})
	require.NoError(t, err)
	revokeID, err := q.EnqueueRevoke(ctx, RevokeArgs{TenantID: queueTenantA, TokenID: 42, Grantee: testGrantee.Hex()})
	require.NoError(t, err)
	otherID, err := q.Enqueue(ctx, ShareArgs{TenantID: queueTenantA, TokenID: 43, Grantee: testGrantee.Hex()})
	require.NoError(t, err)

	s := newVehicleSequencer(q.pool, zerolog.Nop())
	work := func(id int64) (bool, error) {
		row, err := q.Client.JobGet(ctx, id)
		require.NoError(t, err)
		ran := false
		err = s.Work(ctx, row, func(context.Context) error { ran = true; return nil })
		return ran, err
	}

	ran, err := work(revokeID)
	assert.False(t, ran, "the revoke must wait for the upgrade queued before it")
	assertSnoozed(t, err)

	ran, err = work(upgradeID)
	assert.True(t, ran, "the earliest job for a vehicle always runs")
	assert.NoError(t, err)

	ran, err = work(otherID)
	assert.True(t, ran, "a different vehicle does not wait")
	assert.NoError(t, err)

	_, err = q.pool.Exec(ctx,
		`UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, upgradeID)
	require.NoError(t, err)

	ran, err = work(revokeID)
	assert.True(t, ran, "once the upgrade has finished, the revoke runs")
	assert.NoError(t, err)
}

func TestJobTokenID(t *testing.T) {
	id, ok := jobTokenID([]byte(`{"tokenId":190171}`))
	assert.True(t, ok)
	assert.Equal(t, "190171", id)

	_, ok = jobTokenID([]byte(`{"tokenId":null}`))
	assert.False(t, ok)
}
