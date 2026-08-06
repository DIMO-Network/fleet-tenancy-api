package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// EncryptSecret seals plaintext with AES-256-GCM under sha256(passphrase),
// returning base64(nonce|ciphertext). Byte-compatible with fleet-lite-app and
// kaufmann-oracle, which is what lets the backfill read their rows directly.
//
// Note that an empty passphrase is NOT "no encryption" — sha256("") is a valid
// AES-256 key that anyone can compute. config.Settings.Validate refuses to boot
// on one; see docs. Callers must never rely on this function to complain.
func EncryptSecret(passphrase, plaintext string) (string, error) {
	gcm, err := newGCM(passphrase)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(
		append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, []byte(plaintext), nil)...)), nil
}

// DecryptSecret reverses EncryptSecret. GCM authenticates, so a successful
// return is proof the passphrase was correct — a wrong key errors rather than
// yielding garbage, which is what makes key-migration tooling safe.
func DecryptSecret(passphrase, encB64 string) (string, error) {
	if encB64 == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(passphrase)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func newGCM(passphrase string) (cipher.AEAD, error) {
	keyHash := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(keyHash[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
