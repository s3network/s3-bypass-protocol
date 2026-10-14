package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync"
	"testing"
	"time"
)

// newTestCert builds a certificate with a parsed Leaf, because find reads
// Leaf.Subject.CommonName and Leaf.DNSNames. The key material is irrelevant
// here; only the identity fields are inspected.
func newTestCert(names []string) *tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// The hot-reload ticker started by setupOcspTicker replaces entries in the
// certificate list from its own goroutine. That write used to race with the
// scan getNewGetCertificateFunc performs for every TLS handshake. It did not
// reproduce on every run and did not reproduce on some platforms at all, which
// is how it stayed in the tree.
//
// This drives the read and the write from separate goroutines as hard as the
// scheduler allows, so the detector sees it if the two sides share state
// without synchronisation.
func TestCertStoreConcurrentReadWrite(t *testing.T) {
	store := &certStore{}
	for i := 0; i < 8; i++ {
		store.certs = append(store.certs, newTestCert([]string{"a", "b"}[i%2:i%2+1]))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: what the OCSP/hot-reload ticker does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < len(store.certs); i++ {
				store.set(i, newTestCert([]string{"a", "b"}[i%2:i%2+1]))
			}
		}
	}()

	// Readers: what every handshake does.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = store.len()
				_ = store.get(0)
				_ = store.find("a", "*.a")
			}
		}()
	}

	// Give the goroutines time to actually interleave rather than relying on
	// the test goroutine alone.
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// The reload callback must not be able to write outside the slice, and reads
// of an index that no longer exists must return nil rather than panic.
func TestCertStoreOutOfRangeIsSafe(t *testing.T) {
	store := &certStore{}
	if store.get(0) != nil {
		t.Error("expected nil for an empty store")
	}
	store.set(0, newTestCert([]string{"a"})) // ignored, must not grow the slice
	if store.get(0) != nil {
		t.Error("expected nil after a discarded set")
	}
	if store.find("a", "*.a") != nil {
		t.Error("expected no match in an empty store")
	}
}

func TestCertStoreFindMatchesSNIAndWildcard(t *testing.T) {
	store := &certStore{certs: []*tls.Certificate{
		newTestCert([]string{"example.com"}),
		newTestCert([]string{"*.wild.com"}),
	}}
	if cert := store.find("example.com", "*.com"); cert == nil {
		t.Error("exact match not found")
	}
	if cert := store.find("a.wild.com", "*.wild.com"); cert == nil {
		t.Error("wildcard match not found")
	}
	if cert := store.find("nope.example.com", "*.example.com"); cert != nil {
		t.Error("unexpected match")
	}
}
