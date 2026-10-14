// Package s3 implements the Storage interface for S3-compatible object stores.
// Works with AWS S3, MinIO, Backblaze B2, Cloudflare R2, etc.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/xtls/xray-core/proxy/fedarisha/storage"
)

// Config holds S3 connection parameters.
type Config struct {
	Bucket          string // S3 bucket name
	Prefix          string // Key prefix (e.g. "fedarisha/")
	Region          string // AWS region (default "us-east-1")
	Endpoint        string // Custom endpoint for S3-compatible services (MinIO, R2, etc.)
	AccessKey       string
	SecretKey       string
	SkipBucketCheck bool // Prefix-scoped PAKs can use objects but cannot HeadBucket.
}

// S3Store implements storage.Storage for S3-compatible backends.
//
// Reads and writes use SEPARATE s3.Clients with independent HTTP connection
// pools. This is deliberate: fedarisha multiplexes a bidirectional stream over
// S3, and a heavy read flood (GET-ing the peer's data files, plus DELETE-ing
// consumed ones) must never exhaust the connection pool that the write path
// (PUT-ing window-update / data files) depends on. When they shared one pool, a
// sustained download saturated it, starved the window-update PUTs, and the
// peer's yamux send window filled and the whole channel wedged. Splitting the
// pools guarantees the write direction always has connections of its own.
type S3Store struct {
	cfg         Config
	readClient  *s3.Client // GET, List, Delete, HeadBucket, lifecycle (read/cleanup path)
	writeClient *s3.Client // PutObject only (the latency-critical write path)
}

// newHTTPClient builds an HTTP client with a private connection pool sized to
// maxConns. Per-op context timeouts (read/upload) are the real deadline, so
// ResponseHeaderTimeout is only a backstop.
func newHTTPClient(maxConns int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          maxConns,
			MaxIdleConnsPerHost:   maxConns,
			MaxConnsPerHost:       maxConns,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

// New creates a new S3 storage backend.
func New(cfg Config) *S3Store {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}

	newClient := func(hc *http.Client) *s3.Client {
		opts := []func(*s3.Options){
			func(o *s3.Options) {
				o.Region = cfg.Region
				o.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
				o.HTTPClient = hc
				o.Retryer = newStoreRetryer()
			},
		}
		if cfg.Endpoint != "" {
			opts = append(opts, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
				o.UsePathStyle = true
			})
		}
		return s3.New(s3.Options{}, opts...)
	}

	// Read pool covers concurrent GETs (read-ahead window × hedge) and the
	// DELETEs of consumed files; write pool is dedicated to the PUT workers.
	return &S3Store{
		cfg:         cfg,
		readClient:  newClient(newHTTPClient(192)),
		writeClient: newClient(newHTTPClient(192)),
	}
}

func (s *S3Store) key(path string) string {
	path = strings.TrimPrefix(path, "/")
	return s.cfg.Prefix + path
}

// ---------- storage.Storage ----------

func (s *S3Store) Init(ctx context.Context) error {
	if s.cfg.SkipBucketCheck {
		return nil
	}
	// Verify access by doing a HeadBucket.
	_, err := s.readClient.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.cfg.Bucket),
	})
	return err
}

func (s *S3Store) EnsureDir(_ context.Context, _ string) error {
	// S3 has no directories — they're implicit from key prefixes.
	return nil
}

func (s *S3Store) Upload(ctx context.Context, path string, data []byte) error {
	_, err := s.writeClient.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.key(path)),
		Body:   bytes.NewReader(data),
	})
	return err
}

func (s *S3Store) Download(ctx context.Context, path string) ([]byte, error) {
	out, err := s.readClient.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.key(path)),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	// One object is one session file: data files are capped at the writer's
	// maxFileSize and control files are 64 bytes, so nothing legitimate comes
	// near this ceiling. Reading it unbounded meant one oversized object could
	// be made to allocate as much memory as the backend cared to send.
	return readLimited(out.Body, maxObjectBytes)
}

// maxObjectBytes is the hard ceiling on a single object this store will read.
//
// It is a backstop, not a policy: the writer already caps files at maxFileSize,
// so this only fires for an object that is not a fedarisha file at all. A var so
// a test can lower it rather than push 64MB through a loopback server.
var maxObjectBytes = int64(64 << 20)

// readLimited reads at most limit bytes and refuses anything longer.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("object exceeds the %d byte read limit", limit)
	}
	return data, nil
}

func (s *S3Store) List(ctx context.Context, dir string, prefix string) ([]storage.FileInfo, error) {
	dirKey := s.key(dir)
	if !strings.HasSuffix(dirKey, "/") {
		dirKey += "/"
	}

	searchPrefix := dirKey
	if prefix != "" {
		searchPrefix = dirKey + prefix
	}

	var result []storage.FileInfo
	var token *string
	for {
		out, err := s.readClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(searchPrefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}

		for _, obj := range out.Contents {
			name := strings.TrimPrefix(aws.ToString(obj.Key), dirKey)
			if name == "" {
				continue
			}
			var modified time.Time
			if obj.LastModified != nil {
				modified = *obj.LastModified
			}
			result = append(result, storage.FileInfo{
				Name:     name,
				Path:     aws.ToString(obj.Key),
				Size:     aws.ToInt64(obj.Size),
				Modified: modified,
				Created:  modified,
				IsDir:    false,
			})
		}

		for _, cp := range out.CommonPrefixes {
			name := strings.TrimPrefix(aws.ToString(cp.Prefix), dirKey)
			name = strings.TrimSuffix(name, "/")
			if name == "" {
				continue
			}
			result = append(result, storage.FileInfo{
				Name:  name,
				Path:  aws.ToString(cp.Prefix),
				IsDir: true,
			})
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		// A truncated page with no continuation token: the next request would
		// carry a nil token and return this same first page again, so the loop
		// would never terminate. Some S3-compatible backends answer exactly
		// like that at the end of a prefix — stop with what has been listed.
		if out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}

	return result, nil
}

func (s *S3Store) Delete(ctx context.Context, path string) error {
	_, err := s.readClient.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.key(path)),
	})
	return err
}

func (s *S3Store) Watch(ctx context.Context, dir string, since time.Time, timeout time.Duration) ([]storage.FileInfo, error) {
	// S3 has no push notifications — fall back to polling.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		files, err := s.List(ctx, dir, "")
		if err != nil {
			return nil, err
		}
		var newer []storage.FileInfo
		for _, f := range files {
			if f.Modified.After(since) {
				newer = append(newer, f)
			}
		}
		if len(newer) > 0 {
			return newer, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, nil
}

// BatchDelete removes multiple objects in one API call (up to 1000).
func (s *S3Store) BatchDelete(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	objects := make([]s3types.ObjectIdentifier, len(paths))
	for i, p := range paths {
		objects[i] = s3types.ObjectIdentifier{Key: aws.String(s.key(p))}
	}
	out, err := s.readClient.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.cfg.Bucket),
		Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return err
	}
	// Quiet suppresses the per-key SUCCESS elements, not the failures. A
	// backend that answers 200 OK with errors in the body has still refused
	// those keys; reporting success anyway is how a session's objects stayed in
	// the bucket while teardown logged a clean sweep.
	if len(out.Errors) == 0 {
		return nil
	}
	refused := make([]string, 0, len(out.Errors))
	for _, e := range out.Errors {
		refused = append(refused, fmt.Sprintf("%s (%s: %s)",
			aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
	}
	return fmt.Errorf("%d of %d object(s) were NOT deleted: %s", len(refused), len(paths), strings.Join(refused, "; "))
}
