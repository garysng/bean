package sbxtoken

import "testing"

func TestVerifyAcceptsTheMintedToken(t *testing.T) {
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !Verify(Hash(tok), tok) {
		t.Fatal("a freshly minted token did not verify against its own hash")
	}
}

func TestVerifyRejectsAnotherSandboxesToken(t *testing.T) {
	// The property that makes a per-sandbox token a confinement rather than a
	// formality: holding one sandbox's token must not open another's agent.
	mine, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	theirs, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mine == theirs {
		t.Fatal("two calls to New returned the same token")
	}
	if Verify(Hash(theirs), mine) {
		t.Fatal("one sandbox's token verified against another's hash")
	}
}

func TestUnprovisionedGuestRejectsEverything(t *testing.T) {
	// An empty expected hash is what the agent reads when MMDS was never
	// populated. Accepting anything in that state would turn a provisioning
	// failure into an agent with no authentication at all, so it is checked
	// rather than left to the comparison.
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if Verify("", tok) {
		t.Fatal("an unprovisioned hash accepted a token")
	}
	if Verify("", "") {
		t.Fatal("an unprovisioned hash accepted an empty token")
	}
}

func TestMissingCredentialIsRejected(t *testing.T) {
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if Verify(Hash(tok), "") {
		t.Fatal("a request presenting no token was accepted")
	}
}

func TestTheHashDoesNotRevealTheToken(t *testing.T) {
	// What is placed in MMDS must not be usable as a credential. The guest can
	// read it, so if presenting the hash were accepted the whole arrangement
	// would collapse to a shared secret the sandbox already holds.
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := Hash(tok)
	if Verify(h, h) {
		t.Fatal("presenting the hash itself was accepted as the token")
	}
}
