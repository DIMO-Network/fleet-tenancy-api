package main

import (
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A throwaway key generated for this test — never used anywhere real.
const (
	testKeyHex  = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	testAddress = "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
)

func TestDeriveSideRoundTrip(t *testing.T) {
	enc, err := service.EncryptSecret("master-key-a", testKeyHex)
	require.NoError(t, err)

	side, err := deriveSide("master-key-a", "TRAST", "0xstored", enc)
	require.NoError(t, err)

	assert.True(t, side.hasKey)
	assert.Equal(t, testAddress, side.derived.Hex())
	assert.False(t, side.prefixed0x)
	assert.Equal(t, "0xstored", side.stored)
}

// The whole point of the diff: the same key wrapped under two different master
// keys must derive the same address. This is the property that makes comparing
// addresses equivalent to comparing keys, without ever comparing keys.
func TestSameKeyUnderTwoMasterKeysDerivesOneAddress(t *testing.T) {
	encA, err := service.EncryptSecret("kaufmann-master", testKeyHex)
	require.NoError(t, err)
	encB, err := service.EncryptSecret("tenancy-master", testKeyHex)
	require.NoError(t, err)

	a, err := deriveSide("kaufmann-master", "t", "", encA)
	require.NoError(t, err)
	b, err := deriveSide("tenancy-master", "t", "", encB)
	require.NoError(t, err)

	assert.Equal(t, a.derived, b.derived)
}

func TestDeriveSideFlagsThe0xPrefix(t *testing.T) {
	// A 0x-prefixed key derives the SAME address — the prefix is a storage
	// format, not a different key — but the flag must be set, because the share
	// path's HexToECDSA does not trim it.
	enc, err := service.EncryptSecret("k", "0x"+testKeyHex)
	require.NoError(t, err)

	side, err := deriveSide("k", "t", "", enc)
	require.NoError(t, err)
	assert.True(t, side.prefixed0x)
	assert.Equal(t, testAddress, side.derived.Hex())
}

func TestDeriveSideWrongMasterKeyErrs(t *testing.T) {
	// GCM authenticates: the wrong passphrase must error, never yield a wrong
	// address — a wrong address would count as differ and point the
	// investigation at key drift that does not exist.
	enc, err := service.EncryptSecret("right-key", testKeyHex)
	require.NoError(t, err)

	_, err = deriveSide("wrong-key", "t", "", enc)
	assert.Error(t, err)
}

func TestDeriveSideEmptyCiphertextIsNoKeyNotAnError(t *testing.T) {
	side, err := deriveSide("k", "self-serve", "", "")
	require.NoError(t, err)
	assert.False(t, side.hasKey, "a self-serve tenant simply has no signer")
}

func TestUnionKeysIsStableAndComplete(t *testing.T) {
	got := unionKeys(
		map[string]signerSide{"b": {}, "a": {}},
		map[string]signerSide{"c": {}, "b": {}},
	)
	assert.Equal(t, []string{"a", "b", "c"}, got)
}
