// Package sbxtoken mints and verifies the per-sandbox credential that guards a
// sandbox's agent.
//
// The agent used to be unreachable by construction: it listened on AF_VSOCK, a
// host-to-guest address family, so no process inside the sandbox could dial it no
// matter what privileges it held. Serving the agent over TCP -- so that one address
// scheme covers both the agent's port and any port a user exposes -- gives that up,
// and this token is what replaces it.
//
// Two properties are what make it a replacement rather than a formality:
//
// Per sandbox. A single cluster-wide secret would mean that reading it out of one
// sandbox yields a credential for every sandbox on every node. A fresh token per
// sandbox confines a disclosure to the sandbox that disclosed it, which is the one
// whose contents its holder already controls.
//
// The guest holds only a hash. The token reaches the guest through MMDS, which the
// sandbox's own root can read -- so what is placed there is Hash(token), and the
// plaintext stays on the node. That is enough for the agent to check a token
// presented to it, and not enough to construct one.
//
// The hash is not a password hash and deliberately not slow. It authenticates a
// 256-bit random value, so there is no dictionary to iterate and nothing for a work
// factor to buy; making verification expensive would only add latency to every
// request on the data plane.
package sbxtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the size of a minted token. 256 bits, so the token is not guessable
// and the hash below has nothing weaker in front of it.
const tokenBytes = 32

// New mints a token. The plaintext is what noded keeps and presents to the agent;
// only its Hash is given to the guest.
func New() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sbxtoken: read random: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Hash renders the value that is safe to hand to the guest.
//
// Hashing the token's *text* rather than its decoded bytes keeps this function total:
// it has no error case, so a caller cannot be tempted to treat a malformed token as
// an empty one -- and Verify's fixed-size comparison would make that mistake grant
// access to a request that presented nothing.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Verify reports whether presented matches the hash the guest was given.
//
// An empty expected hash rejects everything, which is what a guest needs when MMDS
// was never populated: a provisioning failure must not leave an agent that accepts
// anything. The explicit check below is redundant -- Hash always returns 64
// characters and ConstantTimeCompare reports a length mismatch as unequal -- and is
// kept because it states the intent at the point of decision. The tests pin the
// outcome rather than the mechanism, since it is a security property of this function
// and not an incidental consequence of the comparison it happens to use.
func Verify(expectedHash, presented string) bool {
	if expectedHash == "" || presented == "" {
		return false
	}
	// Constant-time because the comparison is against a secret-derived value and
	// the caller is remote. A byte-at-a-time compare leaks a prefix oracle, which
	// over enough attempts recovers the hash -- and the hash is what the agent
	// accepts, so recovering it is equivalent to holding the token.
	return subtle.ConstantTimeCompare([]byte(Hash(presented)), []byte(expectedHash)) == 1
}
