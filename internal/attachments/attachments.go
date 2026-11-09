// Package attachments provides the S3-compatible, content-addressed attachment core.
package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	DefaultMaxBytes         int64 = 50 << 20
	DefaultStorageCap       int64 = 20_000_000_000
	DefaultPutCap           int64 = 800000
	DefaultGetCap           int64 = 8000000
	DefaultStagingCap       int64 = 1 << 30
	DefaultStagingClientCap int64 = 256 << 20
)

// DefaultMIMEAllow is the MIME allowlist used when HK_ATTACH_MIME_ALLOW is
// unset. The remote MCP attachment_put tool states this set in its description,
// so change both together. image/webp is sniffed by net/http as image/webp
// (RIFF....WEBP), which the image rule requires to match the declared type.
const DefaultMIMEAllow = "image/png,image/jpeg,image/gif,image/webp,text/html,text/plain,text/markdown,application/json,application/pdf,text/csv,application/x-ndjson,text/x-log"

var (
	ErrDisabled         = errors.New("attachments_disabled")
	ErrTooLarge         = errors.New("attachment_too_large")
	ErrMIME             = errors.New("attachment_mime_rejected")
	ErrExtension        = errors.New("attachment_extension_rejected")
	ErrObjectStore      = errors.New("attachment_object_store_unavailable")
	ErrObjectNotFound   = errors.New("attachment_object_not_found")
	ErrChecksumMismatch = errors.New("attachment_checksum_mismatch")
)

type Config struct {
	MaxBytes, StorageCap, PutCap, GetCap                         int64
	Allow                                                        map[string]bool
	Endpoint, Region, Bucket, AccessKey, SecretKey, BucketLookup string
	// StagingCap bounds the declared bytes of direct uploads still charged to the
	// budget (their staging objects may exist). StagingClientCap bounds one
	// creator's share of it, so one client cannot hold the whole budget.
	StagingCap, StagingClientCap int64
	// DirectUpload enables presigned direct upload. It is off unless
	// HK_ATTACH_DIRECT_UPLOAD is 1 or true: the size bound on a presigned PUT
	// depends on the store verifying signed headers, which is confirmed only by
	// the real-bucket negative test (an oversized PUT is refused).
	DirectUpload bool
}

func envBool(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true"
}

func envInt(name string, def int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}
func ConfigFromEnv() (Config, bool, error) { return ConfigFromPrefix("HK") }

// ConfigFromPrefix reads PREFIX_S3_* variables, allowing migration between two
// independently configured object stores without copying credentials into flags.
func ConfigFromPrefix(prefix string) (Config, bool, error) {
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "_")
	if prefix == "" || strings.ContainsAny(prefix, "= \t\n") {
		return Config{}, false, errors.New("invalid object-store environment prefix")
	}
	c := Config{}
	fields := []*string{&c.Endpoint, &c.Region, &c.Bucket, &c.AccessKey, &c.SecretKey, &c.BucketLookup}
	names := []string{prefix + "_S3_ENDPOINT", prefix + "_S3_REGION", prefix + "_S3_BUCKET", prefix + "_S3_ACCESS_KEY_ID", prefix + "_S3_SECRET_ACCESS_KEY", prefix + "_S3_BUCKET_LOOKUP"}
	found := 0
	for i, n := range names {
		*fields[i] = os.Getenv(n)
		if i != 1 && i != 5 && *fields[i] != "" {
			found++
		}
	}
	if found == 0 {
		return c, false, nil
	}
	if found != 4 {
		// A partial deployment must not take down the text API. Treat it as the
		// same disabled state as an intentionally unconfigured instance.
		return c, false, nil
	}
	if c.Region == "" {
		c.Region = "auto"
	}
	if c.BucketLookup == "" {
		c.BucketLookup = "path"
	}
	if c.BucketLookup != "path" && c.BucketLookup != "virtual" {
		return c, false, fmt.Errorf("%s_S3_BUCKET_LOOKUP must be path or virtual", prefix)
	}
	if strings.Contains(strings.ToLower(c.Endpoint), "compat.objectstorage.") {
		if c.Region == "auto" {
			return c, false, fmt.Errorf("%s_S3_REGION is required for OCI Object Storage", prefix)
		}
		// The namespace endpoint serves path-style requests only.
		if c.BucketLookup != "path" {
			return c, false, fmt.Errorf("%s_S3_BUCKET_LOOKUP must be path for OCI Object Storage", prefix)
		}
	}
	var e error
	if c.MaxBytes, e = envInt("HK_ATTACH_MAX_BYTES", DefaultMaxBytes); e != nil {
		return c, false, e
	}
	if c.StorageCap, e = envInt("HK_ATTACH_STORAGE_CAP_BYTES", DefaultStorageCap); e != nil {
		return c, false, e
	}
	if c.PutCap, e = envInt("HK_ATTACH_MONTHLY_PUT_CAP", DefaultPutCap); e != nil {
		return c, false, e
	}
	if c.GetCap, e = envInt("HK_ATTACH_MONTHLY_GET_CAP", DefaultGetCap); e != nil {
		return c, false, e
	}
	if c.StagingCap, e = envInt("HK_ATTACH_STAGING_CAP_BYTES", DefaultStagingCap); e != nil {
		return c, false, e
	}
	if c.StagingClientCap, e = envInt("HK_ATTACH_STAGING_CLIENT_CAP_BYTES", DefaultStagingClientCap); e != nil {
		return c, false, e
	}
	c.DirectUpload = envBool("HK_ATTACH_DIRECT_UPLOAD")
	allow := os.Getenv("HK_ATTACH_MIME_ALLOW")
	if allow == "" {
		allow = DefaultMIMEAllow
	}
	c.Allow = map[string]bool{}
	for _, x := range strings.Split(allow, ",") {
		x = strings.TrimSpace(x)
		if x != "" {
			c.Allow[x] = true
		}
	}
	if len(c.Allow) == 0 {
		return c, false, errors.New("HK_ATTACH_MIME_ALLOW is empty")
	}
	return c, true, nil
}

// ErrObjectNotFound (declared above) is returned by Head when the object does
// not exist. Any other Head error means the object's state is unknown.

// ObjectInfo is an object's size and last-modified time, from HEAD or a listing.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

type ObjectStore interface {
	Stat(context.Context, string) error
	Put(context.Context, string, io.Reader, int64, string) error
	Get(context.Context, string) (io.ReadCloser, error)
	Presign(context.Context, string, time.Duration) (string, error)
	// PresignPut returns a URL that accepts one PUT of the object key with
	// exactly size bytes: the Content-Length is part of the signature where the
	// store honours signed headers.
	PresignPut(context.Context, string, time.Duration, int64) (string, error)
	Head(context.Context, string) (ObjectInfo, error)
	Delete(context.Context, string) error
	List(context.Context, string) ([]ObjectInfo, error)
}
type minioStore struct {
	client *minio.Client
	bucket string
}

func (m minioStore) Stat(ctx context.Context, key string) error {
	_, e := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	return e
}
func (m minioStore) Put(ctx context.Context, key string, r io.Reader, size int64, mime string) error {
	_, e := m.client.PutObject(ctx, m.bucket, key, r, size, minio.PutObjectOptions{ContentType: mime})
	return e
}
func (m minioStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o, e := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if e != nil {
		return nil, e
	}
	if _, e = o.Stat(); e != nil {
		_ = o.Close()
		return nil, e
	}
	return o, nil
}
func (m minioStore) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, e := m.client.PresignedGetObject(ctx, m.bucket, key, ttl, url.Values{})
	return u.String(), e
}
func (m minioStore) PresignPut(ctx context.Context, key string, ttl time.Duration, size int64) (string, error) {
	// Content-Length is signed (SigV4 SignedHeaders), so the URL accepts one
	// body of exactly size bytes; a PUT of any other length fails signature
	// validation on stores that check signed headers.
	h := http.Header{}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	u, e := m.client.PresignHeader(ctx, http.MethodPut, m.bucket, key, ttl, nil, h)
	if e != nil {
		return "", e
	}
	return u.String(), nil
}
func (m minioStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	o, e := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if e != nil {
		if minio.ToErrorResponse(e).Code == "NoSuchKey" {
			return ObjectInfo{}, ErrObjectNotFound
		}
		return ObjectInfo{}, e
	}
	return ObjectInfo{Key: key, Size: o.Size, Modified: o.LastModified}, nil
}
func (m minioStore) Delete(ctx context.Context, key string) error {
	return m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{})
}
func (m minioStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out []ObjectInfo
	for o := range m.client.ListObjects(lctx, m.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, o.Err
		}
		out = append(out, ObjectInfo{Key: o.Key, Size: o.Size, Modified: o.LastModified})
	}
	return out, nil
}

func NewS3(cfg Config) (ObjectStore, error) {
	return newS3(cfg, nil)
}

func newS3(cfg Config, transport http.RoundTripper) (ObjectStore, error) {
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	lookup := minio.BucketLookupPath
	if cfg.BucketLookup == "virtual" {
		lookup = minio.BucketLookupDNS
	}
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	c, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: !strings.HasPrefix(cfg.Endpoint, "http://"), Region: region, BucketLookup: lookup, Transport: transport})
	if e != nil {
		return nil, e
	}
	return minioStore{c, cfg.Bucket}, nil
}

func ObjectLocation(cfg Config) string {
	return strings.TrimRight(cfg.Endpoint, "/") + "/" + cfg.Bucket
}

type Manager struct {
	Store   AttachmentDB
	Objects ObjectStore
	Config  Config
	Enabled bool
	// Now is the clock for upload expiry and the staging sweep; nil means the
	// wall clock. Tests inject it.
	Now func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// AttachmentDB keeps the object transport testable without a live S3 account.
type AttachmentDB interface {
	GetAttachment(context.Context, string) (store.Attachment, bool, error)
	CheckAttachmentCapacity(context.Context, int64, int64, int64) error
	PutAttachment(context.Context, store.Attachment, int64, int64) (store.Attachment, bool, error)
	RecordAttachmentGet(context.Context, string, int64) error
	ListAttachments(context.Context, string, string, int) ([]store.Attachment, error)
	AttachmentUsage(context.Context) (store.AttachmentUsage, error)
	ReserveAttachmentUpload(ctx context.Context, u store.AttachmentUpload, stagingCap, clientCap int64) error
	ListUnreleasedAttachmentUploads(ctx context.Context, createdBefore time.Time, limit int) ([]store.AttachmentUpload, error)
	MarkAttachmentUploadStagingDeleted(ctx context.Context, id string, now time.Time) error
	GetAttachmentUpload(context.Context, string) (store.AttachmentUpload, bool, error)
	TransitionAttachmentUpload(ctx context.Context, id, from, to, reason string, now time.Time) (bool, error)
}

type MigrationDB interface {
	ListAttachmentObjects(context.Context) ([]store.Attachment, error)
	SetAttachmentObjectLocation(context.Context, string, string) error
}

type MigrationResult struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// Migrate copies and verifies all content-addressed objects before recording
// each target location. It never deletes a source object; dry runs only inspect
// attachment metadata. An object already verified at the target is not read
// from the source, so a rerun after the cutover (when new rows exist only at
// the target) completes and picks up rows added after an earlier run's snapshot.
func Migrate(ctx context.Context, db MigrationDB, source, target ObjectStore, targetLocation string, apply bool) (MigrationResult, error) {
	rows, err := db.ListAttachmentObjects(ctx)
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{Objects: int64(len(rows))}
	for _, x := range rows {
		result.Bytes += x.SizeBytes
	}
	if !apply {
		return result, nil
	}
	for _, x := range rows {
		objectKey := key(x.SHA256)
		if _, err := readVerified(ctx, target, objectKey, x.SHA256, x.SizeBytes); err != nil {
			if !objectNotFound(err) {
				return result, fmt.Errorf("target %s: %w", x.SHA256, err)
			}
			body, err := readVerified(ctx, source, objectKey, x.SHA256, x.SizeBytes)
			if err != nil {
				return result, fmt.Errorf("source %s: %w", x.SHA256, err)
			}
			if err = target.Put(ctx, objectKey, bytes.NewReader(body), int64(len(body)), x.MIME); err != nil {
				return result, fmt.Errorf("target put %s: %w", x.SHA256, err)
			}
			if _, err = readVerified(ctx, target, objectKey, x.SHA256, x.SizeBytes); err != nil {
				return result, fmt.Errorf("target %s: %w", x.SHA256, err)
			}
		}
		if x.ObjectLocation == targetLocation {
			continue
		}
		if err := db.SetAttachmentObjectLocation(ctx, x.SHA256, targetLocation); err != nil {
			return result, fmt.Errorf("record target location %s: %w", x.SHA256, err)
		}
	}
	return result, nil
}

func readVerified(ctx context.Context, objects ObjectStore, objectKey, sha string, size int64) ([]byte, error) {
	r, err := objects.Get(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != size || hex.EncodeToString(sum[:]) != sha {
		return nil, ErrChecksumMismatch
	}
	return body, nil
}

func objectNotFound(err error) bool {
	if errors.Is(err, ErrObjectNotFound) {
		return true
	}
	response := minio.ToErrorResponse(err)
	return response.Code == "NoSuchKey" || response.Code == "NotFound"
}

func NewFromEnv(s *store.Store) (*Manager, error) {
	cfg, on, e := ConfigFromEnv()
	if e != nil {
		return nil, e
	}
	m := &Manager{Store: s, Config: cfg, Enabled: on}
	if !on {
		return m, nil
	}
	m.Objects, e = NewS3(cfg)
	return m, e
}
func key(sha string) string { return "hk/" + sha[:2] + "/" + sha }
func isText(m string) bool {
	return strings.HasPrefix(m, "text/") || m == "application/json" || m == "application/x-ndjson"
}
func isImage(m string) bool { return strings.HasPrefix(m, "image/") }
func textSniffAllowed(m string) bool {
	return m == "text/plain" || m == "text/html" || m == "application/json" || m == "application/x-ndjson"
}
func hasNULPrefix(body []byte) bool {
	if len(body) > 8<<10 {
		body = body[:8<<10]
	}
	return bytes.IndexByte(body, 0) >= 0
}

// checkName refuses a file name that is empty, too long, holds a NUL, or has an
// archive or dump extension. It is the part of validate that needs no bytes, so
// a direct upload can refuse a bad declaration before anything is staged.
func checkName(name string) error {
	if name == "" || len(name) > 512 || strings.ContainsRune(name, 0) {
		return errors.New("invalid attachment name")
	}
	ext := strings.ToLower(filepath.Ext(name))
	for _, bad := range []string{".dump", ".sql", ".tar", ".tgz", ".zip", ".gz"} {
		if ext == bad {
			return ErrExtension
		}
	}
	return nil
}

func validate(c Config, name, declared string, body []byte) (string, error) {
	if int64(len(body)) > c.MaxBytes {
		return "", ErrTooLarge
	}
	if err := checkName(name); err != nil {
		return "", err
	}
	sniff := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(body), ";")[0]))
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if declared == "" {
		declared = sniff
	}
	if !c.Allow[declared] {
		return "", ErrMIME
	}
	// Go's sniffer intentionally labels most UTF-8 formats as text/plain. For
	// allowlisted text subtypes the declared type is authoritative, but only when
	// the sniff itself is a known textual type and its first 8 KiB has no NUL.
	if isText(declared) {
		if !textSniffAllowed(sniff) || hasNULPrefix(body) {
			return "", ErrMIME
		}
	} else if isImage(declared) || declared == "application/pdf" {
		if sniff != declared {
			return "", ErrMIME
		}
	} else if sniff != declared {
		return "", ErrMIME
	}
	if isText(declared) {
		if e := guard.Reject(string(body)); e != nil {
			return "", e
		}
	}
	return declared, nil
}
func (m *Manager) Put(ctx context.Context, client, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	if !m.Enabled {
		return store.Attachment{}, false, ErrDisabled
	}
	return m.putBytes(ctx, client, name, mime, ref, body)
}

// putBytes is the one store path for a body: validate, dedupe, capacity, object
// put, then the metadata row. Put and a completed direct upload both call it.
func (m *Manager) putBytes(ctx context.Context, client, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	mime, e := validate(m.Config, name, mime, body)
	if e != nil {
		return store.Attachment{}, false, e
	}
	kind, id, e := ParseRef(ref)
	if e != nil {
		return store.Attachment{}, false, e
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if old, ok, e := m.Store.GetAttachment(ctx, sha); e != nil {
		return old, false, e
	} else if ok {
		old.RefKind, old.RefID = kind, id
		out, new, e := m.Store.PutAttachment(ctx, old, m.Config.StorageCap, m.Config.PutCap)
		return out, new, e
	}
	if e = m.Store.CheckAttachmentCapacity(ctx, int64(len(body)), m.Config.StorageCap, m.Config.PutCap); e != nil {
		return store.Attachment{}, false, e
	}
	k := key(sha)
	if e = m.Objects.Stat(ctx, k); e != nil {
		if e = m.Objects.Put(ctx, k, bytes.NewReader(body), int64(len(body)), mime); e != nil {
			return store.Attachment{}, false, fmt.Errorf("%w: %v", ErrObjectStore, e)
		}
	}
	x := store.Attachment{SHA256: sha, SizeBytes: int64(len(body)), MIME: mime, OriginalName: name, CreatedBy: client, RefKind: kind, RefID: id, ObjectLocation: ObjectLocation(m.Config)}
	out, new, e := m.Store.PutAttachment(ctx, x, m.Config.StorageCap, m.Config.PutCap)
	return out, new, e
}
func (m *Manager) Get(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	if !m.Enabled {
		return store.Attachment{}, nil, ErrDisabled
	}
	x, ok, e := m.Store.GetAttachment(ctx, sha)
	if e != nil {
		return x, nil, e
	}
	if !ok {
		return x, nil, errors.New("attachment_not_found")
	}
	r, e := m.Objects.Get(ctx, key(sha))
	if e != nil {
		return x, nil, fmt.Errorf("%w: %v", ErrObjectStore, e)
	}
	if e = m.Store.RecordAttachmentGet(ctx, sha, m.Config.GetCap); e != nil {
		_ = r.Close()
		return x, nil, e
	}
	return x, r, nil
}
func (m *Manager) Presign(ctx context.Context, sha string) (string, error) {
	if !m.Enabled {
		return "", ErrDisabled
	}
	if _, ok, e := m.Store.GetAttachment(ctx, sha); e != nil {
		return "", e
	} else if !ok {
		return "", errors.New("attachment_not_found")
	}
	u, e := m.Objects.Presign(ctx, key(sha), 10*time.Minute)
	if e != nil {
		return "", fmt.Errorf("%w: %v", ErrObjectStore, e)
	}
	return u, nil
}
func (m *Manager) List(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	if !m.Enabled {
		return nil, ErrDisabled
	}
	if ref == "" {
		return m.Store.ListAttachments(ctx, "", "", limit)
	}
	k, id, e := ParseRef(ref)
	if e != nil {
		return nil, e
	}
	return m.Store.ListAttachments(ctx, k, id, limit)
}
func (m *Manager) Usage(ctx context.Context) (store.AttachmentUsage, error) {
	if !m.Enabled {
		return store.AttachmentUsage{}, ErrDisabled
	}
	return m.Store.AttachmentUsage(ctx)
}
func ParseRef(ref string) (string, string, error) {
	if ref == "" || ref == "none" {
		return "none", "", nil
	}
	if v, ok := strings.CutPrefix(ref, "checkpoint:"); ok {
		return "checkpoint", v, nil
	}
	if v, ok := strings.CutPrefix(ref, "document:"); ok {
		return "document", v, nil
	}
	if v, ok := strings.CutPrefix(ref, "memory:"); ok {
		return "memory", v, nil
	}
	if v, ok := strings.CutPrefix(ref, "task:"); ok {
		if !store.ValidTaskRefID(v) {
			return "", "", errors.New("task ref id must be a positive decimal task id, such as task:12")
		}
		return "task", v, nil
	}
	return "", "", errors.New("ref must be checkpoint:<id>, document:<key>, memory:<agent>/<name>, or task:<id>")
}
