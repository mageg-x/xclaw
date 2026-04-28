package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixMilli(), randomSuffix())
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	if err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}
