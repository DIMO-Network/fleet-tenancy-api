package service

import (
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMinter fails its first failUntil calls and succeeds after.
type fakeMinter struct {
	failUntil int
	calls     int
}

func (f *fakeMinter) GetToken() *jwt.Token {
	f.calls++
	if f.calls <= f.failUntil {
		return nil
	}
	return &jwt.Token{Raw: "minted"}
}

// The case this exists for: the challenge flakes once and the next fresh one
// works. Before the retry this was a 500 to the operator console, or a failed
// nightly diff.
func TestMintWithRetrySucceedsAfterOneFlake(t *testing.T) {
	m := &fakeMinter{failUntil: 1}
	var retried []int

	token := mintWithRetry(m, func(attempt int) { retried = append(retried, attempt) })

	require.NotNil(t, token)
	assert.Equal(t, "minted", token.Raw)
	assert.Equal(t, 2, m.calls, "one failure, one fresh challenge")
	assert.Equal(t, []int{1}, retried, "the retry is logged, not silent")
}

// A credential that is genuinely wrong must still fail, and must fail quickly.
// A retry that hid a real error would turn "your key is wrong" into a hang.
func TestMintWithRetryGivesUp(t *testing.T) {
	m := &fakeMinter{failUntil: 99}

	assert.Nil(t, mintWithRetry(m, nil))
	assert.Equal(t, mintAttempts, m.calls, "bounded, not forever")
}

// The happy path must not pay for the retry: no sleep, no second call.
func TestMintWithRetryFirstAttemptWins(t *testing.T) {
	m := &fakeMinter{}
	retried := false

	require.NotNil(t, mintWithRetry(m, func(int) { retried = true }))
	assert.Equal(t, 1, m.calls)
	assert.False(t, retried)
}

// The last attempt must be a real attempt, not a spare. With three attempts and
// two flakes the third has to be the one that mints — an off-by-one here would
// waste the attempt that most often succeeds.
func TestMintWithRetryUsesEveryAttempt(t *testing.T) {
	m := &fakeMinter{failUntil: mintAttempts - 1}

	require.NotNil(t, mintWithRetry(m, nil))
	assert.Equal(t, mintAttempts, m.calls)
}
