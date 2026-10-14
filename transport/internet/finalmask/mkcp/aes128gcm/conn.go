package aes128gcm

import (
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"net"
	"strconv"

	"github.com/xtls/xray-core/common/crypto"
	"github.com/xtls/xray-core/common/platform"
)

// The key used to be the first 16 bytes of SHA256(password): one hash per
// candidate, so a leaked ciphertext is recovered by brute force at hash speed.
// The current scheme stretches the password with PBKDF2-HMAC-SHA512 and uses
// AES-256.
const (
	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 32 // AES-256
	derivationSalt   = "mkcp/aes128gcm/v2"
)

// allowLegacyKDF lets the writer keep emitting legacy keys while a peer is
// upgraded. Readers always accept both, so only the writer side needs the
// switch during migration.
var allowLegacyKDF = envAllowsLegacy()

func envAllowsLegacy() bool {
	v := platform.NewEnvFlag("MKCP_AESGCM_ALLOW_LEGACY_KDF").GetValue(func() string { return "false" })
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}

// SetAllowLegacyKDF lets configuration pin the key derivation scheme.
func SetAllowLegacyKDF(allow bool) { allowLegacyKDF = allow }

func deriveKey(password string) cipher.AEAD {
	seed, err := pbkdf2.Key(sha512.New, password, []byte(derivationSalt), pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		// pbkdf2.Key only errors on an unusable hash or a key length below
		// the FIPS floor; neither is reachable with the constants above.
		return crypto.NewAesGcm(make([]byte, pbkdf2KeyLen))
	}
	return crypto.NewAesGcm(seed)
}

func deriveLegacyKey(password string) cipher.AEAD {
	hashedPsk := sha256.Sum256([]byte(password))
	return crypto.NewAesGcm(hashedPsk[:16])
}

type aes128gcmConn struct {
	net.PacketConn
	aead cipher.AEAD
	// legacy is always installed: reading is what makes an upgraded listener
	// interoperable with a client that has not been upgraded. It is the AEAD
	// tag that tells the two schemes apart, so trying both on the read path
	// accepts legacy traffic without weakening anything.
	legacy cipher.AEAD
	// writeAEAD is the key used for outgoing datagrams. It follows the
	// current scheme unless the operator pins the legacy one for migration.
	writeAEAD cipher.AEAD
}

func NewConnClient(c *Config, raw net.PacketConn) (net.PacketConn, error) {
	aead := deriveKey(c.Password)
	conn := &aes128gcmConn{PacketConn: raw, aead: aead, legacy: deriveLegacyKey(c.Password), writeAEAD: aead}
	if allowLegacyKDF {
		conn.writeAEAD = conn.legacy
	}
	return conn, nil
}

func NewConnServer(c *Config, raw net.PacketConn) (net.PacketConn, error) {
	return NewConnClient(c, raw)
}

// Size reports the headroom the framing needs. Both schemes are AES-GCM with
// the same nonce and tag sizes, so the current key is representative.
func (c *aes128gcmConn) Size() int {
	return c.aead.NonceSize()
}

func (c *aes128gcmConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	nonceSize := c.aead.NonceSize()
	overhead := c.aead.Overhead()

	// The AEAD tag is the discriminator: whichever key authenticates is the
	// one the peer used, so trying the current key first and the legacy one
	// second is what makes the upgrade backward compatible.
	//
	// Go's AEAD deliberately zeroes the plaintext region when authentication
	// fails, so every attempt needs the original ciphertext restored first.
	ct := append([]byte(nil), p[nonceSize:]...)

	for _, aead := range []cipher.AEAD{c.aead, c.legacy} {
		if aead == nil {
			continue
		}
		if aead.NonceSize() != nonceSize || aead.Overhead() != overhead {
			continue
		}
		copy(p[nonceSize:], ct)
		if _, err := aead.Open(p[nonceSize:nonceSize], p[:nonceSize], p[nonceSize:], nil); err == nil {
			return len(p) - nonceSize - overhead, nil, nil
		}
	}
	return 0, nil, errCannotDecrypt
}

var errCannotDecrypt = decryptError{}

type decryptError struct{}

func (decryptError) Error() string { return "aes128gcm: cannot authenticate datagram" }

func (c *aes128gcmConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	aead := c.writeAEAD
	nonceSize := aead.NonceSize()
	overhead := aead.Overhead()
	if _, err := rand.Read(p[:nonceSize]); err != nil {
		return 0, err
	}
	aead.Seal(p[nonceSize:nonceSize], p[:nonceSize], p[nonceSize:], nil)
	return len(p) + overhead, nil
}
