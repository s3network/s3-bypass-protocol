package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// --- the destructive-operation guard -----------------------------------------

// The regression this pins: `-allow-root fedarisha/b18- -prefix fedarisha/b18-/`
// passed every term of the old predicate (it starts with the root, it is not
// textually equal to it, and after trimming the trailing slash the depth count
// is 1 == 1) — while naming the parent of every live `b18-*/` directory. With
// -delete that is "removed everything the deployment was serving".
func TestRootPlusSlashIsRefused(t *testing.T) {
	cases := []struct {
		prefix string
		root   string
	}{
		{"fedarisha/b18-/", "fedarisha/b18-"},
		{"fedarisha/b18-", "fedarisha/b18-/"},
		{"/fedarisha/b18-/", "/fedarisha/b18-"},
	}
	for _, c := range cases {
		if strictlyInside(c.prefix, c.root) {
			t.Errorf("prefix %q against root %q: the root itself must be refused", c.prefix, c.root)
		}
	}
}

// The other half of the same guard: a prefix strictly under the root, and a
// name-prefix root used as documented in the usage line, are still allowed.
func TestPrefixesStrictlyUnderTheRootAreAllowed(t *testing.T) {
	cases := []struct {
		prefix string
		root   string
	}{
		{"fedarisha/b18-selftest/", "fedarisha/b18-"},
		{"fedarisha/b18-selftest", "fedarisha/b18-"},
		{"fedarisha/b18-a/b18-selftest/", "fedarisha/b18-"},
		{"fedarisha/b18-selftest/", "fedarisha/b18-"},
	}
	for _, c := range cases {
		if !strictlyInside(c.prefix, c.root) {
			t.Errorf("prefix %q against root %q: must be allowed", c.prefix, c.root)
		}
	}
}

// A prefix outside the root, or shallower than it, is refused whatever the
// separator convention — the guard must not have traded one hole for another.
func TestPrefixesOutsideOrShallowAreRefused(t *testing.T) {
	cases := []struct {
		prefix string
		root   string
	}{
		{"other/b18-selftest/", "fedarisha/b18-"},
		{"fedarisha/b18-selftest/", "fedarisha/b19-"},
		{"fedarisha/", "fedarisha/b18-"},
		{"", "fedarisha/b18-"},
		{"/", "/"},
	}
	for _, c := range cases {
		if strictlyInside(c.prefix, c.root) {
			t.Errorf("prefix %q against root %q: must be refused", c.prefix, c.root)
		}
	}
}

// --- Quiet deletes still have to be checked -----------------------------------

// Quiet: true suppresses the SUCCESS elements, not the failures. A backend
// answering 200 OK with an error per key has still refused those keys, and the
// tool used to report the prefix as deleted.
func TestDeleteAllReportsPerKeyErrorsHiddenByQuiet(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Deleted><Key>ok-1</Key></Deleted>
  <Error><Key>locked-1</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>
</DeleteResult>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := deleteAll(context.Background(), c, "bucket", []string{"ok-1", "locked-1"})
	if err == nil {
		t.Fatal("a key the backend refused must not be reported as deleted")
	}
	if !strings.Contains(err.Error(), "locked-1") {
		t.Errorf("the failing key must be named, got %v", err)
	}
}

func TestDeleteAllIsSilentWhenEverythingSucceeded(t *testing.T) {
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Deleted><Key>k</Key></Deleted></DeleteResult>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	if err := deleteAll(context.Background(), newTestClient(t, srv.URL), "bucket", []string{"k"}); err != nil {
		t.Fatalf("a clean delete must not report an error: %v", err)
	}
}

// --- pagination ---------------------------------------------------------------

// IsTruncated with a nil NextContinuationToken: re-issuing with a nil token
// returns page one again, so the loop runs forever on some S3-compatible
// backends. It has to stop with what it has.
func TestListAllStopsWhenTheBackendNeverGivesAContinuationToken(t *testing.T) {
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
  <Contents><Key>fedarisha/b18-selftest/c_0_0</Key><Size>1</Size></Contents>
</ListBucketResult>`))
	}))
	defer srv.Close()

	keys, err := listAll(context.Background(), newTestClient(t, srv.URL), "bucket", "fedarisha/b18-selftest/")
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want the one key the truncated page returned: %v", len(keys), keys)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d list calls, want exactly 1 before giving up on the missing token", n)
	}
}

func newTestClient(t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
	})
}

// audit-check: the exact cases from the audit, evaluated against the real
// implementation rather than a transcription of it.
func TestAuditPrefixGuardCases(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"fedarisha/b18-/", "fedarisha/b18-", false}, // the hole: parent of live data
		{"fedarisha/b18-", "fedarisha/b18-", false},  // the root itself
		{"fedarisha/b18-x/", "fedarisha/b18-", true}, // a legitimate child
		{"fedarisha/", "fedarisha/b18-", false},      // an ancestor
		{"fedarisha/other/", "fedarisha/b18-", false},
		{"", "fedarisha/b18-", false}, // empty prefix is the whole bucket
	}
	for _, c := range cases {
		if got := strictlyInside(c.p, c.root); got != c.want {
			t.Errorf("strictlyInside(%q, %q) = %v, want %v", c.p, c.root, got, c.want)
		}
	}
}
