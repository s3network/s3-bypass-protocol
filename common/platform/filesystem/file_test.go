package filesystem_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/common"
	. "github.com/xtls/xray-core/common/platform/filesystem"
)

func TestStatAssetRejectsInvalidPath(t *testing.T) {
	for _, file := range []string{
		"",
		".",
		"..",
		"../geoip.dat",
		"nested/..",
		"nested/../geoip.dat",
		"nested//geoip.dat",
		"/geoip.dat",
		"/tmp/geoip.dat",
		`C:\geoip.dat`,
		`C:geoip.dat`,
		`\\server\share\geoip.dat`,
		`nested\geoip.dat`,
		`nested\..\geoip.dat`,
		filepath.Join(t.TempDir(), "geoip.dat"),
	} {
		if _, err := StatAsset(file); err == nil {
			t.Fatalf("expected error for %q", file)
		}
	}
}

// A relative certificate path must stay inside the certificate directory,
// otherwise a config can pull in any file of the process.
func TestReadCertRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	certDir := filepath.Join(root, "certs")
	if err := os.Mkdir(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "cert.pem"), []byte("cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.pem")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XRAY_LOCATION_CERT", certDir)

	content, err := ReadCert("cert.pem")
	common.Must(err)
	if string(content) != "cert" {
		t.Fatalf("unexpected content: %q", content)
	}

	for _, file := range []string{
		"",
		".",
		"..",
		"../secret.pem",
		"nested/../secret.pem",
		filepath.Join("..", "secret.pem"),
	} {
		if content, err := ReadCert(file); err == nil {
			t.Fatalf("expected error for %q, but got %q", file, content)
		}
	}
}

// An absolute certificate path is an explicit choice of the operator and keeps
// working.
func TestReadCertAbsolutePath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "absolute.pem")
	if err := os.WriteFile(secret, []byte("absolute"), 0o644); err != nil {
		t.Fatal(err)
	}

	content, err := ReadCert(secret)
	common.Must(err)
	if string(content) != "absolute" {
		t.Fatalf("unexpected content: %q", content)
	}
}
