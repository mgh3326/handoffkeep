package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ditheredImage is a large gradient with a small per-pixel dither. At full
// size a PNG of it is several megabytes, while its 1600 px re-encode
// compresses well under the inline cap — the shape of a screenshot the tool
// must downscale rather than refuse.
func ditheredImage(w, h, amplitude int, seed int64) image.Image {
	rng := rand.New(rand.NewSource(seed))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8(200 + 30*x/w)
			g := uint8(190 + 40*y/h)
			b := uint8(210 - 20*x/w)
			if amplitude > 0 {
				r += uint8(rng.Intn(2*amplitude+1) - amplitude)
				g += uint8(rng.Intn(2*amplitude+1) - amplitude)
				b += uint8(rng.Intn(2*amplitude+1) - amplitude)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngBomb is a PNG whose header declares a huge canvas and carries no image
// data: a decoder would allocate w*h pixels, so the tool must refuse it from
// the header alone.
func pngBomb(w, h uint32) []byte {
	chunk := func(typ string, data []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		crc := crc32.NewIEEE()
		_, _ = crc.Write([]byte(typ))
		_, _ = crc.Write(data)
		_ = binary.Write(&b, binary.BigEndian, crc.Sum32())
		return b.Bytes()
	}
	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // bit depth, RGBA
	out.Write(chunk("IHDR", ihdr))
	out.Write(chunk("IEND", nil))
	return out.Bytes()
}

// storeRow puts bytes in the fake object store with a metadata row, for a type
// the upload allowlist would refuse (video, a generic octet-stream, webp).
func storeRow(db *memAttachmentDB, objs *memObjects, name, mime string, body []byte) string {
	sha := sha256Hex(body)
	db.rows[sha] = store.Attachment{SHA256: sha, SizeBytes: int64(len(body)), MIME: mime, OriginalName: name}
	objs.objects["hk/"+sha[:2]+"/"+sha] = body
	return sha
}

func readTool(t *testing.T, cs *gmcp.ClientSession, sha string) *gmcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: "attachment_read", Arguments: map[string]any{"sha256": sha}})
	if err != nil {
		t.Fatalf("attachment_read: %v", err)
	}
	if res.IsError {
		t.Fatalf("attachment_read is an error result: %s", resultText(res))
	}
	return res
}

func resultText(res *gmcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*gmcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func resultImage(t *testing.T, res *gmcp.CallToolResult) *gmcp.ImageContent {
	t.Helper()
	for _, c := range res.Content {
		if ic, ok := c.(*gmcp.ImageContent); ok {
			return ic
		}
	}
	return nil
}

// A small image is handed over byte for byte, as image content the model can
// see rather than a link.
func TestAttachmentReadReturnsSmallPNGAsImageContent(t *testing.T) {
	cs, _, _, _ := newAttachmentTestServer(t)
	body := encodePNG(t, ditheredImage(24, 16, 0, 1))
	put, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "shot.png", "mime": "image/png", "content_base64": base64Of(body)})
	if err != nil {
		t.Fatal(err)
	}
	res := readTool(t, cs, put["sha256"].(string))
	img := resultImage(t, res)
	if img == nil {
		t.Fatalf("attachment_read returned no image content: %+v", res.Content)
	}
	if img.MIMEType != "image/png" || !bytes.Equal(img.Data, body) {
		t.Fatalf("image content mime=%q bytes=%d, want image/png and the stored %d bytes", img.MIMEType, len(img.Data), len(body))
	}
	if len(res.Content) != 1 {
		t.Fatalf("a small image should be one image block, got %d blocks", len(res.Content))
	}
}

func TestAttachmentReadDownscalesALargePNG(t *testing.T) {
	cs, _, _, _ := newAttachmentTestServer(t)
	body := encodePNG(t, ditheredImage(6000, 1600, 1, 7))
	if len(body) <= attachmentReadImageMaxBytes {
		t.Fatalf("fixture PNG is %d bytes, not above the %d-byte inline cap: it would not exercise the downscale", len(body), attachmentReadImageMaxBytes)
	}
	put, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "wide.png", "mime": "image/png", "content_base64": base64Of(body)})
	if err != nil {
		t.Fatal(err)
	}
	res := readTool(t, cs, put["sha256"].(string))
	img := resultImage(t, res)
	if img == nil {
		t.Fatalf("attachment_read returned no image content: %s", resultText(res))
	}
	if img.MIMEType != "image/png" {
		t.Fatalf("downscaled PNG mime=%q want image/png", img.MIMEType)
	}
	if len(img.Data) > attachmentReadImageMaxBytes {
		t.Fatalf("downscaled image is %d bytes, above the %d-byte cap", len(img.Data), attachmentReadImageMaxBytes)
	}
	out, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatalf("downscaled bytes are not a PNG: %v", err)
	}
	if b := out.Bounds(); b.Dx() > attachmentReadImageLongEdge || b.Dy() > attachmentReadImageLongEdge || b.Dx() != 1600 {
		t.Fatalf("downscaled to %dx%d, want a long edge of %d", b.Dx(), b.Dy(), attachmentReadImageLongEdge)
	}
	note := resultText(res)
	if !strings.Contains(note, "downscaled from 6000x1600") || !strings.Contains(note, "attachment_get_url") {
		t.Fatalf("downscale note %q does not state the original size and the URL tool", note)
	}
}

func TestAttachmentReadDownscalesALargeJPEG(t *testing.T) {
	cs, _, _, _ := newAttachmentTestServer(t)
	body := encodeJPEG(t, ditheredImage(3000, 2000, 24, 11), 90)
	if len(body) <= attachmentReadImageMaxBytes {
		t.Fatalf("fixture JPEG is %d bytes, not above the %d-byte inline cap", len(body), attachmentReadImageMaxBytes)
	}
	put, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "photo.jpg", "mime": "image/jpeg", "content_base64": base64Of(body)})
	if err != nil {
		t.Fatal(err)
	}
	res := readTool(t, cs, put["sha256"].(string))
	img := resultImage(t, res)
	if img == nil {
		t.Fatalf("attachment_read returned no image content: %s", resultText(res))
	}
	if img.MIMEType != "image/jpeg" {
		t.Fatalf("downscaled JPEG mime=%q want image/jpeg", img.MIMEType)
	}
	if len(img.Data) > attachmentReadImageMaxBytes {
		t.Fatalf("downscaled image is %d bytes, above the %d-byte cap", len(img.Data), attachmentReadImageMaxBytes)
	}
	out, err := jpeg.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatalf("downscaled bytes are not a JPEG: %v", err)
	}
	if b := out.Bounds(); b.Dx() > attachmentReadImageLongEdge || b.Dy() > attachmentReadImageLongEdge {
		t.Fatalf("downscaled to %dx%d, want a long edge of at most %d", b.Dx(), b.Dy(), attachmentReadImageLongEdge)
	}
}

// A header that declares a huge canvas is refused before the bitmap is
// allocated, and the refusal says where the file still is.
func TestAttachmentReadRefusesADecodeBombBeforeDecoding(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	body := pngBomb(100000, 100000)
	sha := storeRow(db, objs, "bomb.png", "image/png", body)
	res := readTool(t, cs, sha)
	if resultImage(t, res) != nil {
		t.Fatal("a bomb header came back as image content")
	}
	text := resultText(res)
	if !strings.Contains(text, "megapixel") || !strings.Contains(text, "attachment_get_url") {
		t.Fatalf("bomb refusal %q does not name the pixel limit and the URL tool", text)
	}
}

func TestAttachmentReadReturnsMarkdownAsText(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	body := []byte("# handoff\n\n- one\n- two\n")
	put, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "notes.md", "mime": "text/markdown", "content_base64": base64Of(body)})
	if err != nil {
		t.Fatal(err)
	}
	if got := resultText(readTool(t, cs, put["sha256"].(string))); got != string(body) {
		t.Fatalf("markdown text=%q want %q", got, body)
	}
	// A generic type whose name says text is text too.
	generic := storeRow(db, objs, "rows.tsv", "application/octet-stream", []byte("a\tb\n1\t2\n"))
	if got := resultText(readTool(t, cs, generic)); got != "a\tb\n1\t2\n" {
		t.Fatalf("generic .tsv text=%q", got)
	}
}

// Text over the cap is cut at a UTF-8 boundary and ends with a line naming the
// whole file's size.
func TestAttachmentReadTruncatesLongText(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	var body strings.Builder
	for i := 0; body.Len() < attachmentReadTextMaxBytes+(64<<10); i++ {
		fmt.Fprintf(&body, "line %06d of a log the model only needs the head of\n", i)
	}
	sha := storeRow(db, objs, "run.log", "text/x-log", []byte(body.String()))
	got := resultText(readTool(t, cs, sha))
	shown, note, ok := strings.Cut(got, "\n\n[attachment_read truncated")
	if !ok {
		t.Fatalf("long text was not truncated: %q", got)
	}
	if len(shown) > attachmentReadTextMaxBytes {
		t.Fatalf("showed %d bytes, above the %d-byte cap", len(shown), attachmentReadTextMaxBytes)
	}
	if !strings.HasPrefix(body.String(), shown) {
		t.Fatal("shown text is not a prefix of the file")
	}
	if want := fmt.Sprintf("%d bytes", body.Len()); !strings.Contains(note, want) {
		t.Fatalf("truncation line %q does not name the full size (%s)", note, want)
	}
	if !strings.Contains(note, "attachment_get_url") {
		t.Fatalf("truncation line %q does not point at the URL tool", note)
	}
}

// A cut through a multi-byte rune drops the partial rune instead of returning
// broken UTF-8.
func TestAttachmentReadCutsTextAtAUTF8Boundary(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	body := strings.Repeat("가", attachmentReadTextMaxBytes/3+10)
	sha := storeRow(db, objs, "ko.txt", "text/plain", []byte(body))
	got := resultText(readTool(t, cs, sha))
	shown, _, ok := strings.Cut(got, "\n\n[attachment_read truncated")
	if !ok {
		t.Fatalf("text was not truncated: %d bytes shown", len(got))
	}
	if !utf8.ValidString(shown) {
		t.Fatal("shown text ends in a partial rune")
	}
	// 256 KiB is 1 byte into a 3-byte rune, so the cut keeps 262143 bytes.
	if len(shown) != attachmentReadTextMaxBytes-1 {
		t.Fatalf("shown %d bytes, want the %d-byte boundary", len(shown), attachmentReadTextMaxBytes-1)
	}
	if shown != body[:len(shown)] {
		t.Fatal("shown text is not a prefix of the file")
	}
}

func TestAttachmentReadRefusesNonUTF8Text(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	sha := storeRow(db, objs, "raw.txt", "text/plain", []byte{'a', 0xff, 0xfe, 'b'})
	text := resultText(readTool(t, cs, sha))
	if !strings.Contains(text, "UTF-8") || !strings.Contains(text, "attachment_get_url") {
		t.Fatalf("non-UTF-8 refusal %q does not name the reason and the URL tool", text)
	}
}

// An image already inside the long-edge limit that is still too heavy after
// re-encoding is refused rather than returned in part or over the cap.
func TestAttachmentReadRefusesAnImageThatStillDoesNotFit(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	body := encodePNG(t, ditheredImage(1600, 1200, 24, 13))
	if len(body) <= attachmentReadImageMaxBytes {
		t.Fatalf("fixture PNG is %d bytes, not above the inline cap", len(body))
	}
	sha := storeRow(db, objs, "noisy.png", "image/png", body)
	res := readTool(t, cs, sha)
	if resultImage(t, res) != nil {
		t.Fatal("an image over the cap after re-encoding came back as image content")
	}
	text := resultText(res)
	if !strings.Contains(text, "inline cap") || !strings.Contains(text, "attachment_get_url") {
		t.Fatalf("refusal %q does not name the cap and the URL tool", text)
	}
}

func TestAttachmentReadRefusesVideoAndOtherTypesWithTheURLTool(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	for _, tc := range []struct{ name, mime string }{
		{"clip.mp4", "video/mp4"},
		{"brief.pdf", "application/pdf"},
		{"sound.wav", "audio/wav"},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			sha := storeRow(db, objs, tc.name, tc.mime, []byte("\x00\x01binary\x02"))
			text := resultText(readTool(t, cs, sha))
			if !strings.Contains(text, tc.mime) || !strings.Contains(text, "attachment_get_url") || !strings.Contains(text, sha) {
				t.Fatalf("%s refusal %q does not name the type, the sha and the URL tool", tc.mime, text)
			}
		})
	}
}

// webp has no standard-library decoder: a webp that fits is passed through
// untouched, and one that does not fit is refused rather than downscaled.
func TestAttachmentReadHandlesWebPWithoutADecoder(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	small := []byte("RIFF\x00\x00\x00\x00WEBPVP8 fixture")
	sha := storeRow(db, objs, "small.webp", "image/webp", small)
	img := resultImage(t, readTool(t, cs, sha))
	if img == nil || img.MIMEType != "image/webp" || !bytes.Equal(img.Data, small) {
		t.Fatalf("small webp content=%+v", img)
	}
	large := bytes.Repeat([]byte("RIFF"), attachmentReadImageMaxBytes/2)
	text := resultText(readTool(t, cs, storeRow(db, objs, "large.webp", "image/webp", large)))
	if !strings.Contains(text, "cannot downscale") || !strings.Contains(text, "attachment_get_url") {
		t.Fatalf("oversized webp refusal %q", text)
	}
}

// An unknown sha fails the way attachment_get_url fails, so a client handles
// both the same.
func TestAttachmentReadUnknownSHAIsNotFoundLikeTheURLTool(t *testing.T) {
	cs, _, _, _ := newAttachmentTestServer(t)
	sha := strings.Repeat("0", 64)
	readErr := callToolError(t, cs, "attachment_read", map[string]any{"sha256": sha})
	urlErr := callToolError(t, cs, "attachment_get_url", map[string]any{"sha256": sha})
	if !strings.Contains(readErr, "attachment_not_found") || !strings.Contains(urlErr, "attachment_not_found") {
		t.Fatalf("read err=%q url err=%q, want both attachment_not_found", readErr, urlErr)
	}
}

// The stdio transport reports no size_bytes, so a truncated body is drained to
// learn its full size; a body past the probe bound is reported as a lower
// bound instead of a made-up number.
func TestAttachmentReadTruncationLineOnATransportWithoutSize(t *testing.T) {
	db, objs, m := newAttachmentManager(t)
	cs := connectTestServer(t, New(noSizeBackend{attachmentBackend{spyBackend: &spyBackend{}, m: m}}, "mcp-test"))
	small := strings.Repeat("x", attachmentReadTextMaxBytes+2048)
	sha := storeRow(db, objs, "small.txt", "text/plain", []byte(small))
	note := resultText(readTool(t, cs, sha))
	if !strings.Contains(note, fmt.Sprintf("of %d bytes", len(small))) {
		t.Fatalf("note %q does not name the full size %d", tail(note), len(small))
	}
	// Past the first read and the probe bound, the size is only a lower bound.
	large := strings.Repeat("y", attachmentReadTextProbeMaxBytes+attachmentReadTextMaxBytes+4096)
	sha = storeRow(db, objs, "large.txt", "text/plain", []byte(large))
	note = resultText(readTool(t, cs, sha))
	if !strings.Contains(note, "at least") {
		t.Fatalf("note %q does not mark the size as a lower bound", tail(note))
	}
}

// A body that ends exactly at the probe bound was read whole: the line must
// name the exact size rather than call it a lower bound.
func TestAttachmentReadNamesTheExactSizeAtTheProbeBoundary(t *testing.T) {
	db, objs, m := newAttachmentManager(t)
	cs := connectTestServer(t, New(noSizeBackend{attachmentBackend{spyBackend: &spyBackend{}, m: m}}, "mcp-test"))
	// The first read takes the cap plus one byte, and the probe reaches one
	// byte past its bound before it can tell the stream ended there.
	body := strings.Repeat("z", attachmentReadTextMaxBytes+1+attachmentReadTextProbeMaxBytes+1)
	sha := storeRow(db, objs, "boundary.txt", "text/plain", []byte(body))
	note := resultText(readTool(t, cs, sha))
	if want := fmt.Sprintf("of %d bytes", len(body)); !strings.Contains(note, want) {
		t.Fatalf("note %q does not name the exact size (%s)", tail(note), want)
	}
}

// noSizeBackend hides the stored row's size, as remote.Client does on stdio.
type noSizeBackend struct{ attachmentBackend }

func (b noSizeBackend) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	x, r, err := b.attachmentBackend.GetAttachment(ctx, sha)
	x.SizeBytes = 0
	return x, r, err
}

func callToolError(t *testing.T, cs *gmcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if res.IsError {
		return resultText(res)
	}
	t.Fatalf("%s(%v) succeeded, want an error", name, args)
	return ""
}

// The stdio server reads through /v1/attachments/{sha}, the same route the
// browser and CLI download from.
func TestAttachmentReadOverStdioReadsThroughTheServer(t *testing.T) {
	body := encodePNG(t, ditheredImage(32, 24, 0, 3))
	sha := sha256Hex(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/attachments/"+sha {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="shot.png"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	cs := connectTestServer(t, NewStdio(remote.Client{URL: srv.URL, Token: "fixture-token"}, "mcp-test"))
	img := resultImage(t, readTool(t, cs, sha))
	if img == nil || img.MIMEType != "image/png" || !bytes.Equal(img.Data, body) {
		t.Fatalf("stdio read content=%+v", img)
	}
}

func TestAttachmentReadIsRegisteredOnBothTransportsWithItsContract(t *testing.T) {
	for name, server := range map[string]*gmcp.Server{"http": New(&spyBackend{}, "stdio"), "stdio": NewStdio(&spyBackend{}, "any-token-name")} {
		t.Run(name, func(t *testing.T) {
			cs := connectTestServer(t, server)
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var desc string
			var readOnly bool
			for _, tool := range tools.Tools {
				if tool.Name == "attachment_read" {
					desc = tool.Description
					readOnly = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
				}
			}
			if desc == "" {
				t.Fatal("attachment_read is not registered")
			}
			if !readOnly {
				t.Fatal("attachment_read is not read-only")
			}
			for _, want := range []string{"sha256", "image/png", "image/jpeg", "image/gif", "image/webp", "1.5 MiB", "1600 px", "40 megapixels", "application/json", "256 KiB", "UTF-8", ".md", ".tsv", ".csv", ".txt", "attachment_get_url"} {
				if !strings.Contains(desc, want) {
					t.Errorf("description omits %q", want)
				}
			}
		})
	}
}

func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// tail is the end of a long tool text, for a failure message that stays short.
func tail(s string) string {
	if len(s) <= 200 {
		return s
	}
	return "..." + s[len(s)-200:]
}
