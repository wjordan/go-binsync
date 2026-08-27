package s3

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/wjordan/go-binsync/store"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func TestParseURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw            string
		bucket, prefix string
		wantErr        bool
	}{
		{raw: "s3://b/releases/server", bucket: "b", prefix: "releases/server"},
		{raw: "s3://b/releases/server/", bucket: "b", prefix: "releases/server"},
		{raw: "s3://b/", bucket: "b"},
		{raw: "s3://b", bucket: "b"},
		{raw: "s3://b//releases//", bucket: "b", prefix: "releases"},
		{raw: "s3://my.bucket.name/a b", bucket: "my.bucket.name", prefix: "a b"},
		{raw: "s3:///releases", wantErr: true},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.raw, err)
		}
		bucket, prefix, err := parseURL(u)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseURL(%q): err = %v, want error = %v", tc.raw, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if bucket != tc.bucket || prefix != tc.prefix {
			t.Errorf("parseURL(%q) = %q, %q; want %q, %q", tc.raw, bucket, prefix, tc.bucket, tc.prefix)
		}
	}
}

func TestJoinKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ prefix, key, want string }{
		{"releases/server", "latest.json", "releases/server/latest.json"},
		{"releases/server", "blobs/b3-x.zst", "releases/server/blobs/b3-x.zst"},
		{"", "latest.json", "latest.json"},
	} {
		if got := joinKey(tc.prefix, tc.key); got != tc.want {
			t.Errorf("joinKey(%q, %q) = %q, want %q", tc.prefix, tc.key, got, tc.want)
		}
	}
}

func TestRangeHeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		off, length int64
		want        string
	}{
		{0, 1, "bytes=0-0"},
		{0, 8 << 20, "bytes=0-8388607"},
		{8 << 20, 8 << 20, "bytes=8388608-16777215"},
	} {
		if got := rangeHeader(tc.off, tc.length); got != tc.want {
			t.Errorf("rangeHeader(%d, %d) = %q, want %q", tc.off, tc.length, got, tc.want)
		}
	}
}

func TestPrecondition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		ifMatch      *string
		ifm, ifnm    string
		wantAnyUnset bool
	}{
		{name: "unconditional", ifMatch: nil, wantAnyUnset: true},
		{name: "must not exist", ifMatch: aws.String(""), ifnm: "*"},
		{name: "known etag", ifMatch: aws.String(`"abc"`), ifm: `"abc"`},
	} {
		ifm, ifnm := precondition(tc.ifMatch)
		if tc.wantAnyUnset {
			if ifm != nil || ifnm != nil {
				t.Errorf("%s: got If-Match %v, If-None-Match %v; want both unset", tc.name, ifm, ifnm)
			}
			continue
		}
		if aws.ToString(ifm) != tc.ifm || aws.ToString(ifnm) != tc.ifnm {
			t.Errorf("%s: got If-Match %q, If-None-Match %q; want %q, %q",
				tc.name, aws.ToString(ifm), aws.ToString(ifnm), tc.ifm, tc.ifnm)
		}
	}
}

// respErr builds what the SDK hands back for an S3 error response.
func respErr(status int, code string) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
			Err:      &smithy.GenericAPIError{Code: code, Message: code},
		},
		RequestID: "TESTREQ",
	}
}

func TestMapErr(t *testing.T) {
	t.Parallel()
	plain := errors.New("dial tcp: connection refused")
	for _, tc := range []struct {
		name string
		err  error
		want error // nil means "wrapped, not a sentinel"
	}{
		{"304 poll", respErr(http.StatusNotModified, "NotModified"), store.ErrNotModified},
		{"404 missing object", respErr(http.StatusNotFound, "NoSuchKey"), store.ErrNotFound},
		{"404 head", respErr(http.StatusNotFound, "NotFound"), store.ErrNotFound},
		{"modelled NoSuchKey", &types.NoSuchKey{}, store.ErrNotFound},
		{"412 lost CAS", respErr(http.StatusPreconditionFailed, "PreconditionFailed"), store.ErrPreconditionFailed},
		{"409 raced CAS", respErr(http.StatusConflict, "ConditionalRequestConflict"), store.ErrPreconditionFailed},
		{"404 missing bucket", respErr(http.StatusNotFound, "NoSuchBucket"), nil},
		{"403 denied", respErr(http.StatusForbidden, "AccessDenied"), nil},
		{"500 server", respErr(http.StatusInternalServerError, "InternalError"), nil},
		{"transport", plain, nil},
		{"cancelled", context.Canceled, nil},
	} {
		got := mapErr("get", "releases/server/latest.json", tc.err)
		if tc.want != nil {
			if got != tc.want {
				t.Errorf("%s: mapErr = %v, want %v", tc.name, got, tc.want)
			}
			continue
		}
		for _, sentinel := range []error{store.ErrNotModified, store.ErrNotFound, store.ErrPreconditionFailed} {
			if errors.Is(got, sentinel) {
				t.Errorf("%s: mapErr = %v, want no sentinel (got %v)", tc.name, got, sentinel)
			}
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: mapErr dropped the cause: %v", tc.name, got)
		}
		if !strings.Contains(got.Error(), "releases/server/latest.json") {
			t.Errorf("%s: mapErr lost the key: %v", tc.name, got)
		}
	}
}

func TestParseBoolEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v       string
		want    bool
		wantErr bool
	}{
		{v: "", want: false},
		{v: "1", want: true},
		{v: "true", want: true},
		{v: "TRUE", want: true},
		{v: "0", want: false},
		{v: "false", want: false},
		{v: "yes", wantErr: true},
	} {
		got, err := parseBoolEnv(pathStyleEnv, tc.v)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseBoolEnv(%q): err = %v, want error = %v", tc.v, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseBoolEnv(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// isolate points the SDK at an empty environment so the tests below never read
// the developer's ~/.aws or reach for instance metadata.
func isolate(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AWS_PROFILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", pathStyleEnv} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/credentials")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-east-1")
}

func TestEndpointFromEnv(t *testing.T) {
	isolate(t)
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:9000")
	cfg, err := loadConfig(context.Background())
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := aws.ToString(cfg.BaseEndpoint); got != "http://127.0.0.1:9000" {
		t.Errorf("BaseEndpoint = %q, want the endpoint from AWS_ENDPOINT_URL", got)
	}
}

func TestOpen(t *testing.T) {
	isolate(t)
	for _, tc := range []struct{ raw, want string }{
		{"s3://bucket/releases/server", "s3://bucket/releases/server"},
		{"s3://bucket/releases/server/", "s3://bucket/releases/server"},
		{"s3://bucket", "s3://bucket"},
	} {
		s, err := store.Open(tc.raw)
		if err != nil {
			t.Fatalf("Open(%q): %v", tc.raw, err)
		}
		if got := s.URL(); got != tc.want {
			t.Errorf("Open(%q).URL() = %q, want %q", tc.raw, got, tc.want)
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	t.Setenv(pathStyleEnv, "sure")
	if _, err := store.Open("s3://bucket/x"); err == nil || !strings.Contains(err.Error(), pathStyleEnv) {
		t.Errorf("Open with a bad %s = %v, want that variable named in the error", pathStyleEnv, err)
	}
}

// TestLive runs the shared backend suite against a real endpoint. It needs a
// writable bucket, so it is opt-in:
//
//	BINSYNC_S3_TEST_URL=s3://bucket/go-binsync-test go test ./store/s3/
//
// against AWS, or against MinIO with AWS_ENDPOINT_URL and
// AWS_S3_FORCE_PATH_STYLE=1 set.
func TestLive(t *testing.T) {
	raw := os.Getenv("BINSYNC_S3_TEST_URL")
	if raw == "" {
		t.Skip("set BINSYNC_S3_TEST_URL to a writable bucket to run this")
	}
	s, err := store.Open(raw)
	if err != nil {
		t.Fatalf("open %s: %v", raw, err)
	}
	defer s.Close()
	store.StoreSuite(t, s)
}
