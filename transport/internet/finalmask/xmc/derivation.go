package xmc

import (
	"crypto/pbkdf2"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"math/big"
	"strconv"

	"github.com/xtls/xray-core/common/platform"
)

// Derivation parameters for the current scheme. The password is stretched with
// PBKDF2-HMAC-SHA512 and the key is 2048-bit RSA.
//
// The legacy scheme (DeriveRSAKey) used a single unsalted SHA-256 per prime and
// a 1024-bit modulus. Because the server publishes its public key in the first
// handshake packet, an observer learns N = f(password) and can recover the
// password offline at dictionary speed. The new scheme keeps the same
// "password determines the key" property the protocol needs while making that
// attack cost a full PBKDF2 per candidate.
const (
	// pbkdf2Iterations is deliberately expensive: the derivation runs once per
	// connection setup, not on the data path.
	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 64
	rsaBits          = 2048

	// derivationSalt is a fixed protocol constant, not a secret. It exists so
	// the PBKDF2 input differs from a bare password; per-deployment salts
	// would need a wire change, which the legacy fallback cannot express.
	derivationSalt = "xmc/derivation/v2"
)

// allowLegacyDerivation controls whether DeriveRSAKeyAuto may fall back to the
// legacy scheme. It stays enabled so existing clients keep working; operators
// can pin the new scheme once both ends are upgraded.
var allowLegacyDerivation = envAllowsLegacy()

func envAllowsLegacy() bool {
	v := platform.NewEnvFlag("XMC_ALLOW_LEGACY_DERIVATION").GetValue(func() string { return "true" })
	b, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return b
}

// SetAllowLegacyDerivation lets configuration pin the derivation scheme.
func SetAllowLegacyDerivation(allow bool) { allowLegacyDerivation = allow }

// pbkdf2Seed derives the prime-search seed for the current scheme.
func pbkdf2Seed(password, label string) ([]byte, error) {
	return pbkdf2.Key(sha512.New, password, []byte(derivationSalt+"/"+label), pbkdf2Iterations, pbkdf2KeyLen)
}

// DeriveRSAKeyV2 derives the current 2048-bit RSA private key from a password.
func DeriveRSAKeyV2(password string) (*rsa.PrivateKey, error) {
	pStreamBytes, err := pbkdf2Seed(password, "p-prime")
	if err != nil {
		return nil, err
	}
	qStreamBytes, err := pbkdf2Seed(password, "q-prime")
	if err != nil {
		return nil, err
	}

	p := derivePrimeSized(newSHA256Stream(pStreamBytes), rsaBits/2)
	q := derivePrimeSized(newSHA256Stream(qStreamBytes), rsaBits/2)

	for p.Cmp(q) == 0 {
		q.Add(q, big.NewInt(2))
		q = nextSuitablePrime(q)
	}

	return assembleRSA(p, q)
}

// DeriveRSAKeyAuto returns the key for the current scheme, falling back to the
// legacy derivation so an upgraded build still talks to an un-upgraded peer.
// The returned bool reports whether the legacy scheme was used.
//
// The caller's own public-key comparison is what actually selects the scheme;
// this helper exists so both sides compute the same candidate set.
func DeriveRSAKeyAuto(password string) (key *rsa.PrivateKey, legacy bool, err error) {
	key, err = DeriveRSAKeyV2(password)
	if err == nil && !allowLegacyDerivation {
		return key, false, nil
	}
	if allowLegacyDerivation {
		if legacyKey, legacyErr := DeriveRSAKey(password); legacyErr == nil {
			legacyKey.Precompute()
			return legacyKey, true, nil
		}
	}
	if err != nil {
		return nil, false, err
	}
	return key, false, nil
}

type sha256Stream struct {
	seed    []byte
	counter uint64
	buf     []byte
}

func newSHA256Stream(seed []byte) *sha256Stream {
	return &sha256Stream{
		seed: seed,
	}
}

func (s *sha256Stream) Read(p []byte) (n int, err error) {
	for len(p) > len(s.buf) {
		h := sha256.New()
		h.Write(s.seed)
		h.Write([]byte(fmt.Sprintf("-%d", s.counter)))
		s.counter++
		s.buf = append(s.buf, h.Sum(nil)...)
	}
	n = copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// nextSuitablePrime returns the first odd prime at or after p that is coprime
// to the RSA public exponent.
func nextSuitablePrime(p *big.Int) *big.Int {
	one := big.NewInt(1)
	e := big.NewInt(65537)
	for {
		if p.ProbablyPrime(20) {
			pMinus1 := new(big.Int).Sub(p, one)
			if new(big.Int).GCD(nil, nil, pMinus1, e).Cmp(one) == 0 {
				return p
			}
		}
		p.Add(p, big.NewInt(2))
	}
}

// derivePrimeSized draws a prime of the requested bit length from the stream.
// bits must be a multiple of 8; the top two bits are forced so that the product
// of two such primes has exactly 2*bits.
func derivePrimeSized(stream *sha256Stream, bits int) *big.Int {
	size := bits / 8
	pBytes := make([]byte, size)
	_, _ = stream.Read(pBytes)
	pBytes[0] |= 0xc0
	pBytes[size-1] |= 0x01
	return nextSuitablePrime(new(big.Int).SetBytes(pBytes))
}

func derivePrime(stream *sha256Stream) *big.Int {
	pBytes := make([]byte, 64) // 512 bits
	_, _ = stream.Read(pBytes)
	pBytes[0] |= 0xc0  // ensure it is big enough so p*q is 1024 bits
	pBytes[63] |= 0x01 // ensure odd

	p := new(big.Int).SetBytes(pBytes)
	for {
		if p.ProbablyPrime(20) {
			pMinus1 := new(big.Int).Sub(p, big.NewInt(1))
			e := big.NewInt(65537)
			gcd := new(big.Int).GCD(nil, nil, pMinus1, e)
			if gcd.Cmp(big.NewInt(1)) == 0 {
				return p
			}
		}
		p.Add(p, big.NewInt(2))
	}
}

func assembleRSA(p, q *big.Int) (*rsa.PrivateKey, error) {
	n := new(big.Int).Mul(p, q)
	pMinus1 := new(big.Int).Sub(p, big.NewInt(1))
	qMinus1 := new(big.Int).Sub(q, big.NewInt(1))
	totient := new(big.Int).Mul(pMinus1, qMinus1)

	e := big.NewInt(65537)
	d := new(big.Int).ModInverse(e, totient)
	if d == nil {
		return nil, fmt.Errorf("failed to compute mod inverse")
	}

	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: n,
			E: 65537,
		},
		D:      d,
		Primes: []*big.Int{p, q},
	}
	priv.Precompute()
	return priv, nil
}

// DeriveRSAKey derives a 1024-bit RSA private key from a password.
//
// Deprecated: this is the legacy scheme. It stretches the password with a
// single unsalted SHA-256 per prime, and the resulting 1024-bit modulus is
// published in cleartext in the first handshake packet, so an observer can
// recover the password offline. Use DeriveRSAKeyV2, or DeriveRSAKeyAuto to
// interoperate with peers that have not been upgraded yet.
func DeriveRSAKey(password string) (*rsa.PrivateKey, error) {
	seed := []byte(password)

	pStream := newSHA256Stream(append(seed, []byte("-p-prime")...))
	qStream := newSHA256Stream(append(seed, []byte("-q-prime")...))

	p := derivePrime(pStream)
	q := derivePrime(qStream)

	// ensure p != q (if they are, let's search q further)
	for p.Cmp(q) == 0 {
		q.Add(q, big.NewInt(2))
		q = nextSuitablePrime(q)
	}

	return assembleRSA(p, q)
}
