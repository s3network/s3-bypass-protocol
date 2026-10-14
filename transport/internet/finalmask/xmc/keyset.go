package xmc

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
)

// keySet holds every candidate key for a configured password: the current
// scheme first, plus the legacy scheme as a fallback so that a peer which has
// not been upgraded yet still completes the handshake.
type keySet struct {
	primary     *rsa.PrivateKey
	primaryDER  []byte // PKIX public key, exactly as it appears on the wire
	fallback    *rsa.PrivateKey
	fallbackDER []byte
}

// containerMagic marks the packed representation.
//
// The key material is built from the password locally on each side and is
// stored in config fields that never cross the network, so this encoding only
// has to be understood by the build that produced it. A blob without the magic
// is a bare legacy PKCS#1 key, which is what an older build writes.
var containerMagic = []byte("XMCK1")

// DeriveKeySet derives all candidate keys for a password, current scheme first.
func DeriveKeySet(password string) (*keySet, error) {
	primary, err := DeriveRSAKeyV2(password)
	if err != nil {
		return nil, fmt.Errorf("derive current rsa key: %w", err)
	}
	primaryDER, err := x509.MarshalPKIXPublicKey(&primary.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal current rsa public key: %w", err)
	}

	ks := &keySet{primary: primary, primaryDER: primaryDER}

	if allowLegacyDerivation {
		legacy, legacyErr := DeriveRSAKey(password)
		if legacyErr == nil {
			if legacyDER, derErr := x509.MarshalPKIXPublicKey(&legacy.PublicKey); derErr == nil {
				ks.fallback = legacy
				ks.fallbackDER = legacyDER
			}
		}
	}

	return ks, nil
}

// pack renders the key set into a single blob for the config's
// rsa_private_key field:
//
//	magic | u32 len(primary) | primary | u32 len(fallbackPriv) | fallbackPriv
//	     | u32 len(fallbackPub) | fallbackPub
func (ks *keySet) pack() []byte {
	buf := append([]byte{}, containerMagic...)
	buf = appendBlob(buf, x509.MarshalPKCS1PrivateKey(ks.primary))
	if ks.fallback == nil {
		buf = appendBlob(buf, nil)
		buf = appendBlob(buf, nil)
		return buf
	}
	buf = appendBlob(buf, x509.MarshalPKCS1PrivateKey(ks.fallback))
	buf = appendBlob(buf, ks.fallbackDER)
	return buf
}

// unpackKeySet splits a packed blob back into its keys.
func unpackKeySet(blob []byte) (*keySet, error) {
	if !bytes.HasPrefix(blob, containerMagic) {
		key, err := x509.ParsePKCS1PrivateKey(blob)
		if err != nil {
			return nil, fmt.Errorf("parse rsa private key: %w", err)
		}
		return &keySet{primary: key}, nil
	}

	rest := blob[len(containerMagic):]
	primaryBlob, rest, ok := cutBlob(rest)
	if !ok {
		return nil, fmt.Errorf("packed key set truncated (primary)")
	}
	fallbackPriv, rest, ok := cutBlob(rest)
	if !ok {
		return nil, fmt.Errorf("packed key set truncated (fallback private)")
	}
	fallbackPub, _, ok := cutBlob(rest)
	if !ok {
		return nil, fmt.Errorf("packed key set truncated (fallback public)")
	}

	primary, err := x509.ParsePKCS1PrivateKey(primaryBlob)
	if err != nil {
		return nil, fmt.Errorf("parse current rsa private key: %w", err)
	}
	primaryDER, err := x509.MarshalPKIXPublicKey(&primary.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal current rsa public key: %w", err)
	}

	ks := &keySet{primary: primary, primaryDER: primaryDER}

	if len(fallbackPriv) > 0 && len(fallbackPub) > 0 {
		fallback, err := x509.ParsePKCS1PrivateKey(fallbackPriv)
		if err != nil {
			return nil, fmt.Errorf("parse legacy rsa private key: %w", err)
		}
		ks.fallback = fallback
		ks.fallbackDER = fallbackPub
	}

	return ks, nil
}

// publicKeyFor returns the PKIX public key that config should advertise: the
// current scheme's, so an upgraded peer negotiates it, and the legacy one only
// when the operator pinned the legacy scheme.
func (ks *keySet) publicKeyFor() []byte { return ks.primaryDER }

// PublicKeyFor exposes the advertised public key to configuration code.
func (ks *keySet) PublicKeyFor() []byte { return ks.publicKeyFor() }

// PackedPrivateKey renders both candidate private keys for the config's
// rsa_private_key field.
func (ks *keySet) PackedPrivateKey() []byte { return ks.pack() }

// matchPublicKey selects whichever candidate the peer published. This is the
// negotiation point: before any shared secret exists, the public key is the
// only thing both sides agree on.
func (ks *keySet) matchPublicKey(peer []byte) bool {
	if bytes.Equal(peer, ks.primaryDER) {
		return true
	}
	return ks.fallbackDER != nil && bytes.Equal(peer, ks.fallbackDER)
}

func appendBlob(b, blob []byte) []byte {
	n := uint32(len(blob))
	b = append(b, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(b, blob...)
}

func cutBlob(b []byte) (blob, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}
