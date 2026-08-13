package service

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func refs(ids ...string) []groupAttestationRef {
	out := make([]groupAttestationRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, groupAttestationRef{ID: id, Name: strings.ToUpper(id), Color: "#111111"})
	}
	return out
}

func TestPlanPublishes(t *testing.T) {
	kA := vehKey{tenantID: "tA", tokenID: 1}
	kB := vehKey{tenantID: "tA", tokenID: 2}
	kC := vehKey{tenantID: "tB", tokenID: 3}

	t.Run("first run publishes every grouped vehicle, and only those", func(t *testing.T) {
		pending, res, err := planPublishes(map[vehKey][]groupAttestationRef{
			kA: refs("tA_vans"), kB: refs("tA_vans", "tA_prio"),
		}, map[vehKey]string{})
		require.NoError(t, err)
		assert.Len(t, pending, 2)
		assert.Equal(t, 2, res.Published)
		assert.Zero(t, res.Retracted)
	})

	t.Run("unchanged content is unchanged regardless of run count", func(t *testing.T) {
		pending, res, err := planPublishes(map[vehKey][]groupAttestationRef{kA: refs("tA_vans")}, map[vehKey]string{})
		require.NoError(t, err)
		require.Len(t, pending, 1)

		pending2, res2, err := planPublishes(map[vehKey][]groupAttestationRef{kA: refs("tA_vans")},
			map[vehKey]string{kA: pending[0].digest})
		require.NoError(t, err)
		assert.Empty(t, pending2, "the second run has nothing to do")
		assert.Equal(t, 1, res2.Unchanged)
		assert.Zero(t, res.Retracted)
	})

	t.Run("a rename republishes — the digest covers metadata, not just membership", func(t *testing.T) {
		before, _, err := planPublishes(map[vehKey][]groupAttestationRef{kA: refs("tA_vans")}, map[vehKey]string{})
		require.NoError(t, err)
		renamed := []groupAttestationRef{{ID: "tA_vans", Name: "Renamed", Color: "#111111"}}
		after, res, err := planPublishes(map[vehKey][]groupAttestationRef{kA: renamed},
			map[vehKey]string{kA: before[0].digest})
		require.NoError(t, err)
		require.Len(t, after, 1)
		assert.Equal(t, 1, res.Published)
	})

	t.Run("removal from the last group retracts exactly once", func(t *testing.T) {
		grouped, _, err := planPublishes(map[vehKey][]groupAttestationRef{kC: refs("tB_x")}, map[vehKey]string{})
		require.NoError(t, err)

		// The group is gone; the vehicle has a state row → one empty publish.
		retract, res, err := planPublishes(map[vehKey][]groupAttestationRef{},
			map[vehKey]string{kC: grouped[0].digest})
		require.NoError(t, err)
		require.Len(t, retract, 1)
		assert.Empty(t, retract[0].groups)
		assert.JSONEq(t, `{"groups":[]}`, string(retract[0].dataJSON))
		assert.Equal(t, 1, res.Retracted)

		// Next run: state row holds the empty digest → nothing to do.
		again, res2, err := planPublishes(map[vehKey][]groupAttestationRef{},
			map[vehKey]string{kC: retract[0].digest})
		require.NoError(t, err)
		assert.Empty(t, again)
		assert.Equal(t, 1, res2.Unchanged)
	})

	t.Run("a never-grouped vehicle never publishes", func(t *testing.T) {
		pending, res, err := planPublishes(map[vehKey][]groupAttestationRef{}, map[vehKey]string{})
		require.NoError(t, err)
		assert.Empty(t, pending)
		assert.Zero(t, res.Checked, "no state row and no groups means nothing was ever asserted")
	})
}

// The signature covers exactly the marshaled {"groups":[...]} bytes with the
// id,name,color field order — the contract both source apps' verifiers
// already hold. A drift here is invisible until a third party fails to verify.
func TestGroupsDocumentWireShape(t *testing.T) {
	data, err := json.Marshal(vehicleGroupsDocument{Groups: []groupAttestationRef{
		{ID: "tA_vans", Name: "Vans", Color: "#112233"},
	}})
	require.NoError(t, err)
	assert.Equal(t, `{"groups":[{"id":"tA_vans","name":"Vans","color":"#112233"}]}`, string(data))
}

// signERC191 must produce a personal_sign signature the existing verifiers
// accept: recoverable to the signer's address, V in {27,28}.
func TestSignERC191(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	msg := []byte(`{"groups":[]}`)

	sig, err := signERC191(msg, "0x"+hex.EncodeToString(crypto.FromECDSA(pk)))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sig, "0x"))
	raw, err := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
	require.NoError(t, err)
	require.Len(t, raw, 65)
	assert.Contains(t, []byte{27, 28}, raw[64], "V normalised to the legacy 27/28 form")

	// Recover and compare — the round trip is the whole point.
	raw[64] -= 27
	prefixed := append([]byte("\x19Ethereum Signed Message:\n13"), msg...)
	recovered, err := crypto.SigToPub(crypto.Keccak256(prefixed), raw)
	require.NoError(t, err)
	assert.Equal(t, crypto.PubkeyToAddress(pk.PublicKey), crypto.PubkeyToAddress(*recovered))
}
