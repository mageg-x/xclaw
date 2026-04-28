package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
)

type EncryptedPayload struct {
	CiphertextB64 string
	NonceB64      string
	SaltB64       string
}

func EncryptSecret(masterPassword, plaintext string, iterations, keyLen int) (EncryptedPayload, error) {
	salt := make([]byte, 16)
	if _, err := cryptorand.Read(salt); err != nil {
		return EncryptedPayload{}, fmt.Errorf("generate salt: %w", err)
	}
	key := PBKDF2SHA256([]byte(masterPassword), salt, iterations, keyLen)

	block, err := aes.NewCipher(key)
	if err != nil {
		return EncryptedPayload{}, fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedPayload{}, fmt.Errorf("init gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := cryptorand.Read(nonce); err != nil {
		return EncryptedPayload{}, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return EncryptedPayload{
		CiphertextB64: base64.StdEncoding.EncodeToString(ciphertext),
		NonceB64:      base64.StdEncoding.EncodeToString(nonce),
		SaltB64:       base64.StdEncoding.EncodeToString(salt),
	}, nil
}

func DecryptSecret(masterPassword string, payload EncryptedPayload, iterations, keyLen int) (string, error) {
	salt, err := base64.StdEncoding.DecodeString(payload.SaltB64)
	if err != nil {
		return "", fmt.Errorf("decode salt: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(payload.NonceB64)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload.CiphertextB64)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}

	key := PBKDF2SHA256([]byte(masterPassword), salt, iterations, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("init gcm: %w", err)
	}

	out, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt payload: %w", err)
	}

	return string(out), nil
}
