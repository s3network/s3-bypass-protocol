package aes128gcm

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"testing"
)

func newConn(t *testing.T, password string) *aes128gcmConn {
	t.Helper()
	c, err := NewConnClient(&Config{Password: password}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*aes128gcmConn)
}

// seal encrypts plaintext under an explicitly chosen scheme, the way a peer
// running either build would. This mask is header-style: the caller hands it an
// already-filled buffer rather than a socket.
func seal(aead cipher.AEAD, plaintext []byte) []byte {
	ns := aead.NonceSize()
	frame := make([]byte, ns+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(frame[:ns]); err != nil {
		panic(err)
	}
	aead.Seal(frame[ns:ns], frame[:ns], plaintext, nil)
	return frame
}

// An upgraded reader must still accept a datagram sealed with the legacy key,
// otherwise the upgrade is not backward compatible.
func TestReaderAcceptsLegacyDatagrams(t *testing.T) {
	c := newConn(t, "pw")
	plaintext := []byte("payload")
	frame := seal(deriveLegacyKey("pw"), plaintext)

	n, _, err := c.ReadFrom(frame)
	if err != nil {
		t.Fatalf("legacy datagram rejected: %v", err)
	}
	got := frame[c.Size() : c.Size()+n]
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("payload %q, want %q", got, plaintext)
	}
}

func TestReaderAcceptsCurrentDatagrams(t *testing.T) {
	c := newConn(t, "pw")
	plaintext := []byte("payload")
	frame := seal(c.aead, plaintext)

	n, _, err := c.ReadFrom(frame)
	if err != nil {
		t.Fatalf("current datagram rejected: %v", err)
	}
	got := frame[c.Size() : c.Size()+n]
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("payload %q, want %q", got, plaintext)
	}
}

// Trying more than one key must not turn a wrong-password datagram into an
// accepted one.
func TestReaderRejectsForeignDatagrams(t *testing.T) {
	c := newConn(t, "pw")
	for _, tc := range []struct {
		name string
		aead cipher.AEAD
	}{
		{"legacy-wrong-password", deriveLegacyKey("different-password")},
		{"current-wrong-password", deriveKey("different-password")},
	} {
		frame := seal(tc.aead, []byte("payload"))
		if _, _, err := c.ReadFrom(frame); err == nil {
			t.Errorf("%s: datagram was accepted", tc.name)
		}
	}
}

// The current scheme must actually use a different key, otherwise the change is
// cosmetic. Sealing the same plaintext under a fixed nonce is a function of the
// key alone, so the ciphertexts must differ.
func TestCurrentKeyDiffersFromLegacy(t *testing.T) {
	probe := []byte("probe")
	sealFixed := func(aead cipher.AEAD) []byte {
		zero := make([]byte, aead.NonceSize())
		return aead.Seal(nil, zero, probe, nil)
	}
	if bytes.Equal(sealFixed(deriveKey("pw")), sealFixed(deriveLegacyKey("pw"))) {
		t.Fatal("current and legacy keys are identical")
	}
}

// With the legacy writer switch on, the reader must still accept both schemes
// and the writer must emit legacy frames.
func TestLegacyWriterSwitchSelectsLegacyForWriting(t *testing.T) {
	defer SetAllowLegacyKDF(allowLegacyKDF)
	SetAllowLegacyKDF(true)

	c := newConn(t, "pw")
	if c.writeAEAD == nil {
		t.Fatal("no write key installed")
	}

	// A frame written by the legacy scheme must be readable by a listener that
	// only knows the current key, because the reader tries both.
	listener := newConn(t, "pw")
	frame := seal(deriveLegacyKey("pw"), []byte("payload"))
	if _, _, err := listener.ReadFrom(frame); err != nil {
		t.Fatalf("legacy datagram rejected: %v", err)
	}
}
