package main

import "crypto/rand"

// randReadHelper isolates the crypto/rand.Read call so it's easy to grep
// for "all sources of randomness" in the CLI binary.
func randReadHelper(b []byte) (int, error) { return rand.Read(b) }
