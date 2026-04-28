package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

func PBKDF2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hLen := 32
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)

	for block := 1; block <= blocks; block++ {
		b := make([]byte, len(salt)+4)
		copy(b, salt)
		binary.BigEndian.PutUint32(b[len(salt):], uint32(block))

		u := hmacSHA256(password, b)
		t := make([]byte, hLen)
		copy(t, u)
		for i := 1; i < iterations; i++ {
			u = hmacSHA256(password, u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}

	return out[:keyLen]
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}
