// Package s3 is the s3:// backend for binsync/store. It registers itself from
// init, so only a program that imports it links the AWS SDK:
//
//	import _ "binsync/store/s3"
//
// A store URL is s3://bucket/prefix; an object key k lives at prefix + "/" + k.
// Region and credentials come from the ambient environment (AWS_REGION,
// AWS_ACCESS_KEY_ID, a profile, an instance role, ...) via the SDK's default
// chain, and AWS_ENDPOINT_URL / AWS_ENDPOINT_URL_S3 redirect it at a
// non-AWS endpoint. Set AWS_S3_FORCE_PATH_STYLE=1 for MinIO and other
// endpoints that address buckets by path rather than by hostname.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"binsync/store"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func init() { store.Register("s3", open) }

// pathStyleEnv turns on path-style bucket addressing (MinIO, and any endpoint
// without per-bucket DNS).
const pathStyleEnv = "AWS_S3_FORCE_PATH_STYLE"

type s3store struct {
	c      *awss3.Client
	bucket string
	prefix string
	url    string
}

func open(u *url.URL) (store.Store, error) {
	bucket, prefix, err := parseURL(u)
	if err != nil {
		return nil, err
	}
	pathStyle, err := parseBoolEnv(pathStyleEnv, os.Getenv(pathStyleEnv))
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("s3: loading AWS config: %w", err)
	}
	// SigV4 needs some region even where the endpoint ignores it (MinIO).
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	s := &s3store{
		c:      awss3.NewFromConfig(cfg, func(o *awss3.Options) { o.UsePathStyle = pathStyle }),
		bucket: bucket,
		prefix: prefix,
		url:    "s3://" + bucket,
	}
	if prefix != "" {
		s.url += "/" + prefix
	}
	return s, nil
}

func loadConfig(ctx context.Context) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		// Everything binsync reads is BLAKE3-verified against the pointer, and
		// the SDK's default checksums put an aws-chunked trailer on a streamed
		// upload that GCS's S3 interop endpoint rejects.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
}

// parseURL splits s3://bucket/prefix. The prefix may be empty.
func parseURL(u *url.URL) (bucket, prefix string, err error) {
	if u.Host == "" {
		return "", "", fmt.Errorf("s3: %q: needs a bucket (s3://bucket/prefix)", u)
	}
	return u.Host, strings.Trim(u.Path, "/"), nil
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

func parseBoolEnv(name, v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("s3: %s=%q: not a boolean: %w", name, v, err)
	}
	return b, nil
}

func rangeHeader(off, length int64) string {
	return fmt.Sprintf("bytes=%d-%d", off, off+length-1)
}

// precondition maps PutOptions.IfMatch onto the two conditional-write headers:
// a known ETag is If-Match, "must not exist" is If-None-Match: *.
func precondition(ifMatch *string) (ifm, ifnm *string) {
	switch {
	case ifMatch == nil:
		return nil, nil
	case *ifMatch == "":
		return nil, aws.String("*")
	default:
		return ifMatch, nil
	}
}

func (s *s3store) Get(ctx context.Context, key string, o store.GetOptions) (*store.Object, error) {
	in := &awss3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(joinKey(s.prefix, key))}
	if o.IfNoneMatch != "" {
		in.IfNoneMatch = &o.IfNoneMatch
	}
	if o.Len > 0 {
		in.Range = aws.String(rangeHeader(o.Off, o.Len))
	}
	out, err := s.c.GetObject(ctx, in)
	if err != nil {
		return nil, mapErr("get", *in.Key, err)
	}
	size := int64(-1)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return &store.Object{Body: out.Body, Size: size, ETag: aws.ToString(out.ETag)}, nil
}

func (s *s3store) Put(ctx context.Context, key string, r io.Reader, o store.PutOptions) error {
	full := joinKey(s.prefix, key)
	size := o.Size
	// PutObject signs a Content-Length, so an unsized reader is buffered. Zero
	// is measured rather than trusted: an empty body measures to zero anyway,
	// and a caller that left Size unset would otherwise publish a truncated
	// object under a live key.
	if size <= 0 {
		b, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("s3: put %s: reading body: %w", full, err)
		}
		r, size = bytes.NewReader(b), int64(len(b))
	}
	in := &awss3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &full,
		Body:          r,
		ContentLength: &size,
	}
	if o.ContentType != "" {
		in.ContentType = &o.ContentType
	}
	if o.CacheControl != "" {
		in.CacheControl = &o.CacheControl
	}
	in.IfMatch, in.IfNoneMatch = precondition(o.IfMatch)
	if _, err := s.c.PutObject(ctx, in); err != nil {
		return mapErr("put", full, err)
	}
	return nil
}

func (s *s3store) URL() string { return s.url }

// Close is a no-op: the client borrows the shared HTTP transport.
func (s *s3store) Close() error { return nil }

// mapErr turns an SDK error into the store's sentinels. The HTTP status is the
// signal every S3 implementation agrees on; the error code decides the cases
// where a status alone is ambiguous.
func mapErr(op, key string, err error) error {
	var status int
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	var code string
	var api smithy.APIError
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	switch {
	// A missing bucket is misconfiguration, not a missing object: it must not
	// look like a blob the publisher has yet to upload, which callers retry.
	case code == "NoSuchBucket":
	case status == http.StatusNotModified || code == "NotModified":
		return store.ErrNotModified
	case status == http.StatusNotFound || code == "NoSuchKey" || code == "NotFound":
		return store.ErrNotFound
	// 409 is a conditional write that raced another one; like 412 it is
	// resolved by re-reading the object and trying again.
	case status == http.StatusPreconditionFailed || status == http.StatusConflict ||
		code == "PreconditionFailed":
		return store.ErrPreconditionFailed
	}
	return fmt.Errorf("s3: %s %s: %w", op, key, err)
}
