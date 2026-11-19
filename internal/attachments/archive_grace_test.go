package attachments

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestArchiveGraceFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset is the default", raw: "", want: store.AttachmentArchiveGrace},
		{name: "blank is the default", raw: "   ", want: store.AttachmentArchiveGrace},
		{name: "24h", raw: "24h", want: 24 * time.Hour},
		{name: "168h", raw: "168h", want: 168 * time.Hour},
		{name: "one hour is the floor", raw: "1h", want: time.Hour},
		{name: "not a duration", raw: "7d", wantErr: true},
		{name: "bare number", raw: "24", wantErr: true},
		{name: "below one hour", raw: "59m", wantErr: true},
		{name: "zero", raw: "0s", wantErr: true},
		{name: "negative", raw: "-24h", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HANDOFFKEEP_ATTACH_ARCHIVE_GRACE", tc.raw)
			got, err := ArchiveGraceFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ArchiveGraceFromEnv(%q) = %s, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ArchiveGraceFromEnv(%q) err=%v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ArchiveGraceFromEnv(%q) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}
