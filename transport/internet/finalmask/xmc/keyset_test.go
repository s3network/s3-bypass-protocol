package xmc

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"testing"
)

// The legacy scheme must keep producing exactly the key it always produced, or
// every already-deployed client stops being able to derive the same modulus.
func TestLegacyDerivationIsStable(t *testing.T) {
	// Golden moduli captured from the pre-upgrade implementation.
	for _, tc := range []struct{ password, want string }{
		{
			password: "test",
			want:     "bfac6e720729b437ae0335fca2494179be536070e085cb6b7a33be9775aca092e8e3800aeaafe8ce4c5418336dc8c970550587ba6c0852858d3f08f5d0bec22267cbf2fdd63b19a2c537e862775b258ddaea8e09327af05e67c842aa4cd19281111095a6c94f75f9daf74f70b2feb1ed094ea361dcca91e26b242db5f4d96adb",
		},
		{
			password: "hunter2",
			want:     "aa41d103a3f3e783c849ab7aaba0f877749b2ac858239835d83163d79006dba703dfbc8e985eb6bae8e69824be12d4eaca1ac9e035df3ad9b4b128b06b4a4f204ba31048dce7f004551b6c4e2d4a5a65c6159a7f5e2eb99304d44c86cc9c2918770b9055cf04def510a6aef84edc70de57b6f129986cd3d64a066248421e60bb",
		},
	} {
		key, err := DeriveRSAKey(tc.password)
		if err != nil {
			t.Fatalf("DeriveRSAKey(%q): %v", tc.password, err)
		}
		if got := key.N.Text(16); got != tc.want {
			t.Errorf("DeriveRSAKey(%q) modulus changed:\n got %s\nwant %s", tc.password, got, tc.want)
		}
	}
}

// The new scheme must be a different, stronger key: 2048-bit and stretched.
func TestCurrentDerivationIsStronger(t *testing.T) {
	key, err := DeriveRSAKeyV2("test")
	if err != nil {
		t.Fatal(err)
	}
	if bits := key.N.BitLen(); bits != rsaBits {
		t.Errorf("modulus is %d bits, want %d", bits, rsaBits)
	}
	legacy, err := DeriveRSAKey("test")
	if err != nil {
		t.Fatal(err)
	}
	if key.N.Cmp(legacy.N) == 0 {
		t.Error("current and legacy derivations produced the same key")
	}
}

// The packed container must round-trip, and must keep advertising the current
// scheme's public key so upgraded peers negotiate it.
func TestKeySetPackRoundTrip(t *testing.T) {
	ks, err := DeriveKeySet("test")
	if err != nil {
		t.Fatal(err)
	}
	if ks.fallback == nil {
		t.Fatal("expected a legacy fallback while legacy derivation is allowed")
	}

	packed := ks.PackedPrivateKey()
	got, err := unpackKeySet(packed)
	if err != nil {
		t.Fatal(err)
	}
	if got.primary.N.Cmp(ks.primary.N) != 0 {
		t.Error("primary key did not survive packing")
	}
	if got.fallback == nil || got.fallback.N.Cmp(ks.fallback.N) != 0 {
		t.Error("fallback key did not survive packing")
	}
	if !bytes.Equal(got.publicKeyFor(), ks.publicKeyFor()) {
		t.Error("advertised public key changed across packing")
	}
	if !ks.matchPublicKey(ks.fallbackDER) {
		t.Error("the legacy public key must still be recognised")
	}
	if ks.matchPublicKey([]byte("not a key")) {
		t.Error("an unrelated public key must not match")
	}
}

// A blob without the container magic is what an older build writes; it must
// still parse, as a single legacy key.
func TestUnpackBareLegacyKey(t *testing.T) {
	legacy, err := DeriveRSAKey("test")
	if err != nil {
		t.Fatal(err)
	}
	ks, err := unpackKeySet(x509.MarshalPKCS1PrivateKey(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if ks.primary.N.Cmp(legacy.N) != 0 {
		t.Error("bare legacy key did not parse")
	}
	if ks.fallback != nil {
		t.Error("a bare legacy key must not gain a fallback")
	}
}

// When the operator pins the new scheme, no legacy candidate may be offered.
func TestLegacyCanBeDisabled(t *testing.T) {
	defer SetAllowLegacyDerivation(allowLegacyDerivation)
	SetAllowLegacyDerivation(false)

	ks, err := DeriveKeySet("test")
	if err != nil {
		t.Fatal(err)
	}
	if ks.fallback != nil {
		t.Error("legacy fallback present although disabled")
	}
	if ks.matchPublicKey(ks.primaryDER) != true {
		t.Error("the current public key must still match")
	}
}

// End-to-end property: whatever key a peer advertises, and whichever private
// key we hold, the server must be able to decrypt what the client encrypted to
// that public key.
func TestServerDecryptsWithEitherKey(t *testing.T) {
	ks, err := DeriveKeySet("test")
	if err != nil {
		t.Fatal(err)
	}

	for name, priv := range map[string]*rsa.PrivateKey{"current": ks.primary, "legacy": ks.fallback} {
		peer := &priv.PublicKey
		peerDER, err := x509.MarshalPKIXPublicKey(peer)
		if err != nil {
			t.Fatal(err)
		}
		if !ks.matchPublicKey(peerDER) {
			t.Fatalf("%s: advertised public key was not recognised", name)
		}
		parsed, err := x509.ParsePKIXPublicKey(peerDER)
		if err != nil {
			t.Fatal(err)
		}
		peerRSA, ok := parsed.(*rsa.PublicKey)
		if !ok {
			t.Fatal("not an rsa public key")
		}

		secret := []byte("0123456789abcdef")
		ct, err := rsa.EncryptPKCS1v15(rand.Reader, peerRSA, secret)
		if err != nil {
			t.Fatal(err)
		}

		c := &serverConn{rsaPrivateKey: ks.primary, rsaFallbackKey: ks.fallback}
		got, err := c.decrypt(ct)
		if err != nil {
			t.Fatalf("%s: decrypt failed: %v", name, err)
		}
		if !bytes.Equal(got, secret) {
			t.Fatalf("%s: decrypted %q, want %q", name, got, secret)
		}
	}
}
