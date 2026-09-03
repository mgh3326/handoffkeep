// Package attachments provides the R2-backed, content-addressed attachment core.
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
	DefaultMaxBytes   int64 = 50 << 20
	DefaultStorageCap int64 = 8 << 30
	DefaultPutCap     int64 = 800000
	DefaultGetCap     int64 = 8000000
)

var (
	ErrDisabled  = errors.New("attachments_disabled")
	ErrTooLarge  = errors.New("attachment_too_large")
	ErrMIME      = errors.New("attachment_mime_rejected")
	ErrExtension = errors.New("attachment_extension_rejected")
	ErrR2        = errors.New("attachment_r2_unavailable")
)

type Config struct {
	MaxBytes, StorageCap, PutCap, GetCap   int64
	Allow                                  map[string]bool
	Endpoint, Bucket, AccessKey, SecretKey string
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
func ConfigFromEnv() (Config, bool, error) {
	c := Config{}
	fields := []*string{&c.Endpoint, &c.Bucket, &c.AccessKey, &c.SecretKey}
	names := []string{"HK_S3_ENDPOINT", "HK_S3_BUCKET", "HK_S3_ACCESS_KEY_ID", "HK_S3_SECRET_ACCESS_KEY"}
	found := 0
	for i, n := range names {
		*fields[i] = os.Getenv(n)
		if *fields[i] != "" {
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
	allow := os.Getenv("HK_ATTACH_MIME_ALLOW")
	if allow == "" {
		allow = "image/png,image/jpeg,image/gif,text/html,text/plain,text/markdown,application/json,application/pdf,text/csv,application/x-ndjson,text/x-log"
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

type ObjectStore interface {
	Stat(context.Context, string) error
	Put(context.Context, string, io.Reader, int64, string) error
	Get(context.Context, string) (io.ReadCloser, error)
	Presign(context.Context, string, time.Duration) (string, error)
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
func NewR2(cfg Config) (ObjectStore, error) {
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	c, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: !strings.HasPrefix(cfg.Endpoint, "http://"), Region: "auto", BucketLookup: minio.BucketLookupPath})
	if e != nil {
		return nil, e
	}
	return minioStore{c, cfg.Bucket}, nil
}

type Manager struct {
	Store   AttachmentDB
	Objects ObjectStore
	Config  Config
	Enabled bool
}

// AttachmentDB keeps the object transport testable without a live R2 account.
type AttachmentDB interface {
	GetAttachment(context.Context, string) (store.Attachment, bool, error)
	CheckAttachmentCapacity(context.Context, int64, int64, int64) error
	PutAttachment(context.Context, store.Attachment, int64, int64) (store.Attachment, bool, error)
	RecordAttachmentGet(context.Context, string, int64) error
	ListAttachments(context.Context, string, string, int) ([]store.Attachment, error)
	AttachmentUsage(context.Context) (store.AttachmentUsage, error)
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
	m.Objects, e = NewR2(cfg)
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
func validate(c Config, name, declared string, body []byte) (string, error) {
	if int64(len(body)) > c.MaxBytes {
		return "", ErrTooLarge
	}
	if name == "" || len(name) > 512 || strings.ContainsRune(name, 0) {
		return "", errors.New("invalid attachment name")
	}
	ext := strings.ToLower(filepath.Ext(name))
	for _, bad := range []string{".dump", ".sql", ".tar", ".tgz", ".zip", ".gz"} {
		if ext == bad {
			return "", ErrExtension
		}
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
			return store.Attachment{}, false, fmt.Errorf("%w: %v", ErrR2, e)
		}
	}
	x := store.Attachment{SHA256: sha, SizeBytes: int64(len(body)), MIME: mime, OriginalName: name, CreatedBy: client, RefKind: kind, RefID: id}
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
		return x, nil, fmt.Errorf("%w: %v", ErrR2, e)
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
		return "", fmt.Errorf("%w: %v", ErrR2, e)
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
	return "", "", errors.New("ref must be checkpoint:<id>, document:<key>, or memory:<agent>/<name>")
}
