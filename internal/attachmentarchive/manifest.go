package attachmentarchive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ManifestName is the file each task directory holds.
const ManifestName = "manifest.json"

// Manifest is one task directory's manifest: the archive copies this task
// refers to, in sha order.
type Manifest struct {
	Attachments []ManifestEntry `json:"attachments"`
}

// ManifestEntry is one archived attachment as the manifest records it. Path
// is relative to the archive root, so a shared sha has one file and every
// referring task's entry names it. Source is "hk" for a copy downloaded
// through hk's attachment route.
type ManifestEntry struct {
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	MIME         string    `json:"mime"`
	OriginalName string    `json:"original_name"`
	Path         string    `json:"path"`
	ArchivedAt   time.Time `json:"archived_at"`
	Source       string    `json:"source"`
}

// upsertManifest writes entry into dir/manifest.json, keeping an existing
// entry's archived_at for the same sha. An unreadable or malformed manifest
// stops the write instead of being replaced: an operator decides what a
// damaged manifest means.
func upsertManifest(dir string, entry ManifestEntry) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, ManifestName)
	var m Manifest
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("manifest_unreadable: %s: %v", p, err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return err
	}
	for i := range m.Attachments {
		if m.Attachments[i].SHA256 == entry.SHA256 {
			entry.ArchivedAt = m.Attachments[i].ArchivedAt
			m.Attachments[i] = entry
			return writeManifest(dir, m)
		}
	}
	m.Attachments = append(m.Attachments, entry)
	sort.SliceStable(m.Attachments, func(i, j int) bool { return m.Attachments[i].SHA256 < m.Attachments[j].SHA256 })
	return writeManifest(dir, m)
}

// writeManifest replaces dir/manifest.json atomically: a temp file in the same
// directory, fsynced, then renamed over the manifest.
func writeManifest(dir string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(b); err != nil {
		removeTemp()
		return err
	}
	if err := tmp.Sync(); err != nil {
		removeTemp()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, ManifestName)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
