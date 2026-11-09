package attachments

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfigFromPrefixDefaultsAndOCIRegion(t *testing.T) {
	for _, name := range []string{"R2_S3_ENDPOINT", "R2_S3_BUCKET", "R2_S3_ACCESS_KEY_ID", "R2_S3_SECRET_ACCESS_KEY", "R2_S3_REGION", "R2_S3_BUCKET_LOOKUP", "OCI_S3_ENDPOINT", "OCI_S3_BUCKET", "OCI_S3_ACCESS_KEY_ID", "OCI_S3_SECRET_ACCESS_KEY", "OCI_S3_REGION", "OCI_S3_BUCKET_LOOKUP"} {
		t.Setenv(name, "")
	}
	t.Setenv("R2_S3_ENDPOINT", "https://r2.example")
	t.Setenv("R2_S3_BUCKET", "files")
	t.Setenv("R2_S3_ACCESS_KEY_ID", "key")
	t.Setenv("R2_S3_SECRET_ACCESS_KEY", "secret")
	r2, enabled, err := ConfigFromPrefix("R2")
	if err != nil || !enabled {
		t.Fatalf("R2 config: enabled=%v err=%v", enabled, err)
	}
	if r2.Region != "auto" || r2.BucketLookup != "path" || DefaultStorageCap != 20_000_000_000 {
		t.Fatalf("R2 defaults: %+v cap=%d", r2, DefaultStorageCap)
	}
	t.Setenv("OCI_S3_ENDPOINT", "https://namespace.compat.objectstorage.us-ashburn-1.oraclecloud.com")
	t.Setenv("OCI_S3_BUCKET", "files")
	t.Setenv("OCI_S3_ACCESS_KEY_ID", "key")
	t.Setenv("OCI_S3_SECRET_ACCESS_KEY", "secret")
	if _, _, err = ConfigFromPrefix("OCI"); err == nil || !strings.Contains(err.Error(), "OCI_S3_REGION is required") {
		t.Fatalf("missing OCI region error = %v", err)
	}
	t.Setenv("OCI_S3_REGION", "us-ashburn-1")
	oci, enabled, err := ConfigFromPrefix("OCI")
	if err != nil || !enabled || oci.Region != "us-ashburn-1" || oci.BucketLookup != "path" {
		t.Fatalf("OCI config=%+v enabled=%v err=%v", oci, enabled, err)
	}
	t.Setenv("OCI_S3_ENDPOINT", "https://NS.COMPAT.OBJECTSTORAGE.us-ashburn-1.ORACLECLOUD.COM")
	t.Setenv("OCI_S3_REGION", "")
	if _, _, err = ConfigFromPrefix("OCI"); err == nil || !strings.Contains(err.Error(), "OCI_S3_REGION is required") {
		t.Fatalf("upper-case OCI endpoint skipped the region guard: %v", err)
	}
	t.Setenv("OCI_S3_ENDPOINT", "https://namespace.compat.objectstorage.us-ashburn-1.oraclecloud.com")
	t.Setenv("OCI_S3_REGION", "us-ashburn-1")
	t.Setenv("OCI_S3_BUCKET_LOOKUP", "virtual")
	if _, _, err = ConfigFromPrefix("OCI"); err == nil || !strings.Contains(err.Error(), "must be path for OCI") {
		t.Fatalf("virtual OCI lookup error = %v", err)
	}
}

func TestNewS3UsesEndpointRegionAndPathOrVirtualLookup(t *testing.T) {
	for _, tc := range []struct{ name, lookup, endpoint, wantHost, wantPath string }{
		{"path", "path", "https://objects.example", "objects.example", "/bucket/hk/ab/abc"},
		{"virtual", "virtual", "https://objects.example", "bucket.objects.example", "/hk/ab/abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotHost, gotPath, gotAuth string
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotHost, gotPath, gotAuth = r.URL.Host, r.URL.EscapedPath(), r.Header.Get("Authorization")
				h := make(http.Header)
				h.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
				h.Set("ETag", `"abc"`)
				h.Set("Content-Length", "0")
				return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			cfg := Config{Endpoint: tc.endpoint, Region: "us-ashburn-1", Bucket: "bucket", AccessKey: "access", SecretKey: "secret", BucketLookup: tc.lookup}
			objects, err := newS3(cfg, transport)
			if err != nil {
				t.Fatal(err)
			}
			if err = objects.Stat(context.Background(), "hk/ab/abc"); err != nil {
				t.Fatal(err)
			}
			if gotHost != tc.wantHost || gotPath != tc.wantPath {
				t.Fatalf("request host/path = %q %q", gotHost, gotPath)
			}
			if !strings.Contains(gotAuth, "/us-ashburn-1/s3/aws4_request") {
				t.Fatalf("authorization does not sign configured region: %q", gotAuth)
			}
		})
	}
}

func TestObjectLocationOmitsCredentials(t *testing.T) {
	cfg := Config{Endpoint: "https://namespace.compat.objectstorage.region.oraclecloud.com/", Bucket: "files", AccessKey: "secret-id", SecretKey: "secret"}
	location := ObjectLocation(cfg)
	if strings.Contains(location, "secret") || location != "https://namespace.compat.objectstorage.region.oraclecloud.com/files" {
		t.Fatalf("location = %q", location)
	}
	if _, err := url.Parse(location); err != nil {
		t.Fatal(err)
	}
}
