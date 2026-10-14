package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestStore(t *testing.T, endpoint string) *S3Store {
	t.Helper()
	return New(Config{
		Bucket:          "bucket",
		Endpoint:        endpoint,
		AccessKey:       "ak",
		SecretKey:       "sk",
		SkipBucketCheck: true,
	})
}

// Quiet: true suppresses the SUCCESS elements, not the failures. A backend
// answering 200 OK with an error per key has still refused those keys; the
// store reported success anyway, so cleanupSession logged a clean sweep over
// objects that were still in the bucket.
func TestBatchDeleteReportsPerKeyErrorsHiddenByQuiet(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Deleted><Key>sessions/a/c_0_0</Key></Deleted>
  <Error><Key>sessions/a/c_0_1</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>
</DeleteResult>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	err := newTestStore(t, srv.URL).BatchDelete(context.Background(), []string{"sessions/a/c_0_0", "sessions/a/c_0_1"})
	if err == nil {
		t.Fatal("a key the backend refused must not be reported as deleted")
	}
	if !strings.Contains(err.Error(), "sessions/a/c_0_1") {
		t.Errorf("the failing key must be named, got %v", err)
	}
}

func TestBatchDeleteIsSilentWhenEverythingSucceeded(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>k</Key></Deleted></DeleteResult>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	if err := newTestStore(t, srv.URL).BatchDelete(context.Background(), []string{"k"}); err != nil {
		t.Fatalf("a clean batch delete must not report an error: %v", err)
	}
}

// IsTruncated with a nil NextContinuationToken: the next request carries a nil
// token and returns this same page again, so the loop never ends. Some
// S3-compatible backends answer exactly like that at the end of a prefix.
func TestListStopsWhenTheBackendNeverGivesAContinuationToken(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() > 5 {
			t.Errorf("ListObjectsV2 called %d times: pagination is looping", calls.Load())
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <IsTruncated>true</IsTruncated>
  <Contents><Key>u/sessions/a/c_0_0</Key><Size>10</Size></Contents>
</ListBucketResult>`))
	}))
	defer srv.Close()

	files, err := newTestStore(t, srv.URL).List(context.Background(), "u/sessions/a", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want the one the truncated page returned: %+v", len(files), files)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d list calls, want exactly 1 before giving up on the missing token", n)
	}
}

// A single object must not be able to allocate whatever the backend feels like
// sending: Download is used on the read path for files the writer already caps
// at maxFileSize, so anything past the ceiling is not a fedarisha file.
func TestDownloadRefusesAnOversizedObject(t *testing.T) {
	restore := maxObjectBytes
	maxObjectBytes = 1024
	t.Cleanup(func() { maxObjectBytes = restore })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	store := newTestStore(t, srv.URL)
	if _, err := store.Download(context.Background(), "u/sessions/a/c_0_0"); err == nil {
		t.Fatal("an object past the read limit must be refused")
	}
}

func TestReadLimitedAcceptsExactlyTheLimit(t *testing.T) {
	data, err := readLimited(strings.NewReader("12345"), 5)
	if err != nil {
		t.Fatalf("a body of exactly the limit must be accepted: %v", err)
	}
	if string(data) != "12345" {
		t.Fatalf("got %q, want the whole body", data)
	}
	if _, err := readLimited(strings.NewReader("123456"), 5); err == nil {
		t.Fatal("one byte past the limit must be refused")
	}
}

// The notification client is shared: a fresh Transport per call left a private
// connection pool behind on every SetupWebhook.
func TestWebhookClientIsReused(t *testing.T) {
	if webhookHTTPClient() != webhookHTTPClient() {
		t.Fatal("every notification call must share one client, not build its own transport")
	}
}
