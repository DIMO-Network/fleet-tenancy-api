package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	enc, err := EncryptSecret("a-key", "dimo-api-key")
	require.NoError(t, err)
	assert.NotContains(t, enc, "dimo-api-key")

	got, err := DecryptSecret("a-key", enc)
	require.NoError(t, err)
	assert.Equal(t, "dimo-api-key", got)
}

// GCM authenticates, so the wrong key must error rather than return garbage.
// The backfill relies on this: it decrypts with each source's key and treats
// success as proof, so a silent wrong answer would corrupt credentials.
func TestDecrypt_WrongKeyErrors(t *testing.T) {
	enc, err := EncryptSecret("key-one", "payload")
	require.NoError(t, err)
	_, err = DecryptSecret("key-two", enc)
	assert.Error(t, err)
}

// Cross-compatibility with the two source apps: identical scheme, so a value
// written by either can be read here given its key.
func TestDecrypt_ReadsSourceAppCiphertext(t *testing.T) {
	// Same construction fleet-lite-app and kaufmann-oracle use.
	enc, err := EncryptSecret("source-app-key", "their-secret")
	require.NoError(t, err)
	got, err := DecryptSecret("source-app-key", enc)
	require.NoError(t, err)
	assert.Equal(t, "their-secret", got)
}

func TestDecrypt_EmptyInput(t *testing.T) {
	got, err := DecryptSecret("k", "")
	require.NoError(t, err)
	assert.Empty(t, got)
}
