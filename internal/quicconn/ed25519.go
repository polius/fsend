package quicconn

import (
	"crypto/ed25519"
	"crypto/rand"
)

// generateEd25519 returns a fresh ed25519 keypair for use in a session cert.
//
// Ed25519 is widely supported by TLS 1.3 stacks and produces tiny certs,
// keeping handshake latency low.
func generateEd25519() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	return pub, priv, err
}
