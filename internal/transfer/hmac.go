package transfer

import (
	"crypto/hmac"
	"crypto/sha256"
)

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
