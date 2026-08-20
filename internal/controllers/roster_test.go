package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A caller building its list by concatenating overlapping sets — the entitled
// set and a group's vehicles, say — must not get the same vehicle twice in the
// response. Deduped on the way in rather than on the way out, so the query is
// the size of the distinct set too.
func TestDedupeTokenIDs(t *testing.T) {
	assert.Equal(t, []int64{3, 1, 2}, dedupeTokenIDs([]int64{3, 1, 3, 2, 1, 3}),
		"repeats collapse, the caller's order survives")
	assert.Empty(t, dedupeTokenIDs(nil))
	assert.Equal(t, []int64{7}, dedupeTokenIDs([]int64{7, 7, 7}))
}
