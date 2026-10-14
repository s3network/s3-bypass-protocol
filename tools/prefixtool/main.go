// prefixtool lists and removes S3 objects under explicitly named prefixes.
//
// It exists because "clean up the test keys" must not be able to touch a
// production prefix by accident: deletion requires an explicit --delete, is
// restricted to prefixes under --allow-root, and refuses a prefix that is
// shorter than the allowed root. Dry run is the default.
//
// Credentials come from a fedarisha client config; no value is ever printed.
//
//	go run ./tools/prefixtool -config config.json -allow-root fedarisha/b18- -prefix fedarisha/b18-selftest/
//	go run ./tools/prefixtool -config config.json -allow-root fedarisha/b18- -prefix fedarisha/b18-selftest/ -delete
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fedarishaConfig struct {
	Outbounds []struct {
		Protocol string `json:"protocol"`
		Settings struct {
			Storage struct {
				Type      string `json:"type"`
				Bucket    string `json:"bucket"`
				Endpoint  string `json:"endpoint"`
				Region    string `json:"region"`
				AccessKey string `json:"accessKey"`
				SecretKey string `json:"secretKey"`
			} `json:"storage"`
		} `json:"settings"`
	} `json:"outbounds"`
}

func main() {
	var (
		configPath = flag.String("config", "", "path to a fedarisha client config (credentials source)")
		allowRoot  = flag.String("allow-root", "", "only prefixes under this root may be touched")
		listRoots  = flag.Bool("list-roots", false, "list top-level prefixes present in the bucket and exit")
		del        = flag.Bool("delete", false, "actually delete (default is a dry run)")
	)
	var prefixes multiFlag
	flag.Var(&prefixes, "prefix", "prefix to inspect or delete (repeatable)")
	flag.Parse()

	if *configPath == "" || *allowRoot == "" {
		fmt.Fprintln(os.Stderr, "error: --config and --allow-root are required")
		os.Exit(2)
	}

	cfg, err := loadStorage(*configPath)
	if err != nil {
		fail(err)
	}

	client := newClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if *listRoots {
		roots, err := topLevelPrefixes(ctx, client, cfg.bucket)
		if err != nil {
			fail(err)
		}
		fmt.Printf("top-level prefixes in %s:\n", cfg.bucket)
		for _, r := range roots {
			fmt.Printf("  %s\n", r)
		}
		return
	}

	if len(prefixes) == 0 {
		fail(fmt.Errorf("no --prefix given; use --list-roots to see what is in the bucket"))
	}

	for _, p := range prefixes {
		// Safety: never act outside the allowed root, and never on a prefix
		// that could be a parent of live data.
		if !strictlyInside(p, *allowRoot) {
			fail(fmt.Errorf("refusing prefix %q: it is not strictly inside %q", p, *allowRoot))
		}
	}

	total := 0
	for _, p := range prefixes {
		keys, err := listAll(ctx, client, cfg.bucket, p)
		if err != nil {
			fail(err)
		}
		fmt.Printf("%s: %d objects\n", p, len(keys))
		total += len(keys)
		if !*del {
			continue
		}
		if len(keys) == 0 {
			continue
		}
		if err := deleteAll(ctx, client, cfg.bucket, keys); err != nil {
			fail(fmt.Errorf("delete under %s: %w", p, err))
		}
		fmt.Printf("%s: deleted\n", p)
	}

	fmt.Printf("total objects: %d (mode: %s)\n", total, map[bool]string{true: "DELETE", false: "dry run"}[*del])
	if !*del {
		fmt.Println("nothing was removed — re-run with -delete to actually delete")
	}
}

type storage struct {
	bucket, endpoint, region, accessKey, secretKey string
}

// normalizePrefix strips the leading and trailing separators so that "a/b",
// "a/b/" and "/a/b" are one and the same prefix. Every containment comparison
// runs on these forms: comparing the raw strings made "fedarisha/b18-" and
// "fedarisha/b18-/" look like different prefixes even though they name the
// same place.
func normalizePrefix(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

// strictlyInside reports whether p is strictly contained in root.
//
// It exists because the guard it replaced had a hole in exactly the place it
// was written to protect. The old predicate was
//
//	!strings.HasPrefix(p, root) || p == root ||
//	    count(trimSlash(p)) < count(trimSlash(root))
//
// and `-allow-root fedarisha/b18- -prefix fedarisha/b18-/` passed all three
// terms: the prefix does start with the root, it is not textually equal to it,
// and after trimming the trailing slash the depth count is 1 == 1. That
// prefix is the parent of every live `b18-*/` directory, so `-delete` would
// have reported "deleted" after removing all of them.
//
// The rule is therefore stated on the normalised forms and requires a strict
// extension of the root — equality excluded, which is what closes the hole.
// The old depth comparison is gone because strict extension already implies
// it: any string that has the root as a proper prefix has at least as many
// separators as the root, never fewer.
func strictlyInside(p, root string) bool {
	pn, rn := normalizePrefix(p), normalizePrefix(root)
	if pn == "" {
		return false // an empty prefix is the whole bucket
	}
	if pn == rn {
		return false // the root itself, with or without a trailing slash
	}
	return strings.HasPrefix(pn, rn)
}

func loadStorage(path string) (storage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return storage{}, fmt.Errorf("read config: %w", err)
	}
	var doc fedarishaConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		return storage{}, fmt.Errorf("parse config: %w", err)
	}
	for _, o := range doc.Outbounds {
		if o.Protocol != "fedarisha" {
			continue
		}
		s := o.Settings.Storage
		if s.Bucket == "" || s.AccessKey == "" {
			continue
		}
		return storage{bucket: s.Bucket, endpoint: s.Endpoint, region: s.Region, accessKey: s.AccessKey, secretKey: s.SecretKey}, nil
	}
	return storage{}, fmt.Errorf("no fedarisha outbound with storage credentials in the config")
}

func newClient(s storage) *s3.Client {
	region := s.region
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.Region = region
			o.Credentials = credentials.NewStaticCredentialsProvider(s.accessKey, s.secretKey, "")
		},
	}
	if s.endpoint != "" {
		base := s.endpoint
		if !strings.HasPrefix(base, "http") {
			base = "https://" + base
		}
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(base)
			o.UsePathStyle = true
		})
	}
	return s3.New(s3.Options{}, opts...)
}

func listAll(ctx context.Context, c *s3.Client, bucket, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			return keys, nil
		}
		// IsTruncated without a continuation token: re-issuing the request
		// with a nil token returns the same first page forever, so the loop
		// would never end. Some S3-compatible backends answer this way at the
		// end of a large prefix; stop with what has been collected.
		if out.NextContinuationToken == nil {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

func topLevelPrefixes(ctx context.Context, c *s3.Client, bucket string) ([]string, error) {
	var out []string
	var token *string
	for {
		res, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Delimiter: aws.String("/"), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, cp := range res.CommonPrefixes {
			out = append(out, aws.ToString(cp.Prefix))
		}
		if !aws.ToBool(res.IsTruncated) {
			return out, nil
		}
		if res.NextContinuationToken == nil {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

func deleteAll(ctx context.Context, c *s3.Client, bucket string, keys []string) error {
	const chunk = 1000 // DeleteObjects limit
	var refused []string
	for i := 0; i < len(keys); i += chunk {
		end := min(i+chunk, len(keys))
		ids := make([]types.ObjectIdentifier, 0, end-i)
		for _, k := range keys[i:end] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
		// Quiet suppresses the per-key SUCCESS elements, not the failures. A
		// backend that answers 200 OK with errors in the body has still
		// refused those keys — reading only the status line and declaring the
		// prefix deleted is how objects that survived a delete end up reported
		// as gone.
		for _, e := range out.Errors {
			refused = append(refused, fmt.Sprintf("%s (%s: %s)",
				aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%d of %d object(s) were NOT deleted: %s", len(refused), len(keys), strings.Join(refused, "; "))
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
