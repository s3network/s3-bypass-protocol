package fedarisha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fedstorage "github.com/xtls/xray-core/proxy/fedarisha/storage"
	fedtransport "github.com/xtls/xray-core/proxy/fedarisha/transport"
)

// noSetupStore is an s3-shaped backend that cannot set up notifications. It is
// what makes registerWebhook take the "autoSetup requires s3 storage" branch
// after the hub has already been counted.
type noSetupStore struct{}

func (noSetupStore) Init(context.Context) error                   { return nil }
func (noSetupStore) EnsureDir(context.Context, string) error      { return nil }
func (noSetupStore) Upload(context.Context, string, []byte) error { return nil }
func (noSetupStore) Download(context.Context, string) ([]byte, error) {
	return nil, errors.New("not found")
}

func (noSetupStore) List(context.Context, string, string) ([]fedstorage.FileInfo, error) {
	return nil, nil
}
func (noSetupStore) Delete(context.Context, string) error { return nil }
func (noSetupStore) Watch(context.Context, string, time.Time, time.Duration) ([]fedstorage.FileInfo, error) {
	return nil, nil
}

func testWebhookCfg(listen, publicURL string, autoSetup bool) *WebhookConfig {
	return &WebhookConfig{
		Enabled:   true,
		Listen:    listen,
		PublicUrl: publicURL,
		AutoSetup: autoSetup,
	}
}

func testStorageCfg(prefix string) *StorageConfig {
	return &StorageConfig{Type: "s3", Bucket: "bucket", Prefix: prefix}
}

// A failed registration must not leave the endpoint counting a hub that no
// caller can release. The refcount never reached zero (so the listener was
// never shut down) and the orphaned hub kept its prefix in group.hubs, which
// made validatePrefix reject every later inbound wanting an overlapping prefix
// on that endpoint — the second inbound was refused for a registration that
// had already failed.
func TestFailedRegistrationDoesNotLeakTheHub(t *testing.T) {
	const listen = "127.0.0.1:0"
	const publicURL = "http://127.0.0.1:9/webhook"

	if _, _, err := registerWebhook(context.Background(), "in-1", testStorageCfg("user-a/"), noSetupStore{}, testWebhookCfg(listen, publicURL, true)); err == nil {
		t.Fatal("autoSetup against a store that cannot do it must fail")
	}

	if refs := endpointRefs(listen); refs != 0 {
		t.Fatalf("endpoint refcount is %d after a failed registration, want 0", refs)
	}

	// The endpoint is still reusable: a second inbound with a different prefix
	// must be accepted, and the failed one must not be standing in its way.
	hub, closeWebhook, err := registerWebhook(context.Background(), "in-2", testStorageCfg("user-b/"), noSetupStore{}, testWebhookCfg(listen, publicURL, false))
	if err != nil {
		t.Fatalf("a second inbound must be accepted after a failed one: %v", err)
	}
	if hub == nil || closeWebhook == nil {
		t.Fatal("a successful registration must hand back a hub and its cleanup")
	}
	closeWebhook()

	if refs := endpointRefs(listen); refs != 0 {
		t.Fatalf("endpoint refcount is %d after the cleanup ran, want 0", refs)
	}
}

// endpointRefs is the number of hubs currently counted against a listen
// address, or 0 once the last one has gone and the endpoint with it.
func endpointRefs(listen string) int {
	sharedWebhooks.Lock()
	defer sharedWebhooks.Unlock()
	if endpoint := sharedWebhooks.endpoints[listen]; endpoint != nil {
		return endpoint.refs
	}
	return 0
}

// The registration that follows the failed one uses an OVERLAPPING prefix on
// purpose. An orphaned hub from the failed registration would still be in
// group.hubs and validatePrefix would refuse it — so this pins the same leak
// from the side the operator actually noticed it on.
func TestFailedRegistrationDoesNotBlockALaterOverlappingPrefix(t *testing.T) {
	const listen = "127.0.0.1:0"
	const publicURL = "http://127.0.0.1:9/webhook"

	if _, _, err := registerWebhook(context.Background(), "in-1", testStorageCfg("user-a/"), noSetupStore{}, testWebhookCfg(listen, publicURL, true)); err == nil {
		t.Fatal("autoSetup against a store that cannot do it must fail")
	}

	hub, closeWebhook, err := registerWebhook(context.Background(), "in-2", testStorageCfg("user-a/sub/"), noSetupStore{}, testWebhookCfg(listen, publicURL, false))
	if err != nil {
		t.Fatalf("a prefix nested inside a FAILED registration must be accepted: %v", err)
	}
	if hub == nil {
		t.Fatal("no hub was returned")
	}
	closeWebhook()
}

// The handler is a public endpoint and read its body with a bare io.ReadAll, so
// a request that never stopped sending grew the process until it died.
func TestWebhookHandlerRejectsAnOversizedBody(t *testing.T) {
	group := &webhookGroup{publicURL: "http://127.0.0.1:9/webhook", hubs: map[*fedtransport.WebhookHub]registeredWebhook{}}

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(strings.Repeat("x", webhookMaxBodyBytes+1)))
	rec := httptest.NewRecorder()
	group.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for a body past the limit", rec.Code, http.StatusBadRequest)
	}
}

func TestWebhookHandlerAcceptsANormalBody(t *testing.T) {
	group := &webhookGroup{publicURL: "http://127.0.0.1:9/webhook", hubs: map[*fedtransport.WebhookHub]registeredWebhook{}}

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"Records":[]}`))
	rec := httptest.NewRecorder()
	group.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
