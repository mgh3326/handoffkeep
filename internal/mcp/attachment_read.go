package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// attachmentReadImageMaxBytes caps the encoded image bytes one response
	// carries. A larger image is downscaled and re-encoded, then checked
	// against this cap again.
	attachmentReadImageMaxBytes = 1536 << 10
	// attachmentReadImageMaxPixels refuses a decode bomb from its header,
	// before the image is decoded. 40 MP is far above a screenshot and bounds
	// the decoded bitmap to about 160 MiB.
	attachmentReadImageMaxPixels = 40_000_000
	// attachmentReadImageMaxSourceBytes bounds the object bytes an inline
	// image may occupy. Decoding needs the whole file in memory, so a larger
	// object is refused before it is read.
	attachmentReadImageMaxSourceBytes = 64 << 20
	// attachmentReadImageLongEdge is the long edge a downscaled image keeps.
	attachmentReadImageLongEdge = 1600
	// attachmentReadTextMaxBytes is how much text one response carries.
	attachmentReadTextMaxBytes = 256 << 10
	// attachmentReadTextProbeMaxBytes bounds how far the rest of a truncated
	// text body is drained to learn its full size, for a transport that does
	// not carry the stored row's size_bytes. The probe reads two bytes past
	// it: a count that stops exactly on a limit cannot be told apart from one
	// that stops short of more.
	attachmentReadTextProbeMaxBytes = 4 << 20
)

// attachmentReadDescription states the inline contract and names the URL tool
// as the fallback, so a refusal is always actionable.
var attachmentReadDescription = "Read one attachment's content inline, so the file itself is in the conversation instead of behind a link. " +
	"sha256 is the content hash attachment_list reports. " +
	"image/png, image/jpeg, image/gif and image/webp return image content; an image above 1.5 MiB encoded is downscaled to a 1600 px long edge and re-encoded (PNG, or JPEG q85 for a JPEG), and one that still does not fit, one whose header declares more than 40 megapixels, and one whose bytes do not decode are not returned inline. " +
	"text/*, application/json, and a generic type whose name ends .md, .tsv, .csv or .txt return text up to 256 KiB, cut at a UTF-8 boundary with a line naming the full size; bytes that are not valid UTF-8 are not returned inline. " +
	"Anything else — video, audio, PDF, another image type — returns a text block instead. " +
	"Every refusal names attachment_get_url, which mints a ten-minute private URL for the whole file."

// attachmentArchivedAnswer is the tool answer for an attachment whose hot copy
// is archived: where it is, and that there is no object to read or sign. It
// reports false for any other error.
func attachmentArchivedAnswer(sha string, err error) (map[string]any, bool) {
	var archived *attachments.ArchivedError
	if !errors.As(err, &archived) {
		return nil, false
	}
	if archived.SHA256 == "" {
		archived.SHA256 = sha
	}
	return map[string]any{
		"sha256":   archived.SHA256,
		"archived": true,
		"location": archived.Location,
		"note":     "attachment " + archived.SHA256 + " is archived at " + archived.Location + "; hk holds no hot copy, so nothing is read or signed. Restore is not available yet.",
	}, true
}

// attachmentRead returns the tool result for one attachment body. The caller
// closes body.
func attachmentRead(x store.Attachment, body io.Reader) (*gmcp.CallToolResult, any, error) {
	mimeType := attachmentMIMEType(x.MIME)
	switch {
	case isInlineImageMIME(mimeType):
		return attachmentReadImage(x, mimeType, body)
	case isInlineTextMIME(mimeType, x.OriginalName):
		return attachmentReadText(x, body)
	default:
		return attachmentReadRefusal(x, "its type "+displayMIME(mimeType)+" is not shown inline")
	}
}

// attachmentReadRefusal answers with the reason and the one tool that can
// still deliver the file. It is content rather than a tool error, so the model
// reads the reason and continues with the URL.
func attachmentReadRefusal(x store.Attachment, why string) (*gmcp.CallToolResult, any, error) {
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{
		Text: fmt.Sprintf("attachment %s is not returned inline: %s. Call attachment_get_url with sha256 %q for a ten-minute private URL to the whole file.", x.SHA256, why, x.SHA256),
	}}}, nil, nil
}

// attachmentMIMEType is the stored type without parameters, lower-cased.
func attachmentMIMEType(m string) string {
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}

func displayMIME(m string) string {
	if m == "" {
		return "(none)"
	}
	return m
}

// isInlineImageMIME lists the image types MCP clients render. webp is
// returned as-is while it fits the cap; only png, jpeg and gif can also be
// downscaled, because the standard library decodes those.
func isInlineImageMIME(m string) bool {
	switch m {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func isDownscalableImageMIME(m string) bool {
	switch m {
	case "image/png", "image/jpeg", "image/gif":
		return true
	}
	return false
}

// isInlineTextMIME reports whether the type is text this tool returns as text:
// text/*, application/json, and a generic type whose file name says it is
// text. The name matters only for a generic type, where the mime carries no
// format at all.
func isInlineTextMIME(m, name string) bool {
	if m == "application/json" || strings.HasPrefix(m, "text/") {
		return true
	}
	if m != "" && m != "application/octet-stream" && m != "binary/octet-stream" {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".tsv", ".csv", ".txt":
		return true
	}
	return false
}

func attachmentReadImage(x store.Attachment, mimeType string, body io.Reader) (*gmcp.CallToolResult, any, error) {
	if x.SizeBytes > attachmentReadImageMaxSourceBytes {
		return attachmentReadRefusal(x, fmt.Sprintf("it is %d bytes, above the %d MiB this server reads to show an image inline", x.SizeBytes, attachmentReadImageMaxSourceBytes>>20))
	}
	src, err := io.ReadAll(io.LimitReader(body, attachmentReadImageMaxSourceBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(src) > attachmentReadImageMaxSourceBytes {
		return attachmentReadRefusal(x, fmt.Sprintf("it is larger than the %d MiB this server reads to show an image inline", attachmentReadImageMaxSourceBytes>>20))
	}
	// The bomb guard reads only the header: a declared size over the pixel
	// cap is refused before the bitmap is allocated.
	if isDownscalableImageMIME(mimeType) {
		config, _, err := image.DecodeConfig(bytes.NewReader(src))
		if err != nil {
			return attachmentReadRefusal(x, "its bytes do not decode as "+mimeType)
		}
		if int64(config.Width)*int64(config.Height) > attachmentReadImageMaxPixels {
			return attachmentReadRefusal(x, fmt.Sprintf("its header declares %d megapixels (%dx%d), above the %d megapixel decode limit, so it was not decoded", int64(config.Width)*int64(config.Height)/1_000_000, config.Width, config.Height, attachmentReadImageMaxPixels/1_000_000))
		}
	}
	if len(src) <= attachmentReadImageMaxBytes {
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.ImageContent{MIMEType: mimeType, Data: src}}}, nil, nil
	}
	if !isDownscalableImageMIME(mimeType) {
		return attachmentReadRefusal(x, fmt.Sprintf("it is %d bytes and this server cannot downscale %s", len(src), mimeType))
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return attachmentReadRefusal(x, "its bytes do not decode as "+mimeType)
	}
	scaled := scaleToLongEdge(img, attachmentReadImageLongEdge)
	encoded, outMIME, err := encodeInlineImage(scaled, mimeType)
	if err != nil {
		return nil, nil, err
	}
	if len(encoded) > attachmentReadImageMaxBytes {
		return attachmentReadRefusal(x, fmt.Sprintf("it is %d bytes and is still %d bytes after re-encoding at a %d px long edge, above the %d MiB inline cap", len(src), len(encoded), attachmentReadImageLongEdge, attachmentReadImageMaxBytes>>20))
	}
	content := []gmcp.Content{&gmcp.ImageContent{MIMEType: outMIME, Data: encoded}}
	// The bytes are not the original: say how they differ, so the model does
	// not read detail that was scaled or re-encoded away as if it were at full
	// resolution, and so the original stays reachable.
	b, s := img.Bounds(), scaled.Bounds()
	if b.Dx() != s.Dx() || b.Dy() != s.Dy() {
		content = append(content, &gmcp.TextContent{Text: fmt.Sprintf("image downscaled from %dx%d to %dx%d and re-encoded to fit the %d MiB inline cap; call attachment_get_url with sha256 %q for the original.", b.Dx(), b.Dy(), s.Dx(), s.Dy(), attachmentReadImageMaxBytes>>20, x.SHA256)})
	} else {
		how := "re-encoded as " + outMIME
		if outMIME == "image/jpeg" {
			how += " at quality 85"
		}
		content = append(content, &gmcp.TextContent{Text: fmt.Sprintf("image %s to fit the %d MiB inline cap; call attachment_get_url with sha256 %q for the original.", how, attachmentReadImageMaxBytes>>20, x.SHA256)})
	}
	return &gmcp.CallToolResult{Content: content}, nil, nil
}

// scaleToLongEdge shrinks an image so its long edge is at most longEdge, with
// an area-average filter. It returns src unchanged when it already fits.
func scaleToLongEdge(src image.Image, longEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= longEdge && h <= longEdge {
		return src
	}
	nw, nh := longEdge, h*longEdge/w
	if h > w {
		nh, nw = longEdge, w*longEdge/h
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0 := b.Min.Y + y*h/nh
		y1 := b.Min.Y + (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := b.Min.X + x*w/nw
			x1 := b.Min.X + (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					// RGBA is alpha-premultiplied, which is what averaging
					// needs; the result is un-premultiplied for NRGBA.
					r, g, bl, a := src.At(xx, yy).RGBA()
					sr, sg, sb, sa = sr+uint64(r), sg+uint64(g), sb+uint64(bl), sa+uint64(a)
					n++
				}
			}
			i := dst.PixOffset(x, y)
			if sa == 0 {
				dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = 0, 0, 0, 0
				continue
			}
			dst.Pix[i] = uint8(sr * 0xffff / sa >> 8)
			dst.Pix[i+1] = uint8(sg * 0xffff / sa >> 8)
			dst.Pix[i+2] = uint8(sb * 0xffff / sa >> 8)
			dst.Pix[i+3] = uint8(sa / n >> 8)
		}
	}
	return dst
}

// encodeInlineImage re-encodes a downscaled image in its own format, except a
// gif, which becomes a png still.
func encodeInlineImage(img image.Image, mimeType string) ([]byte, string, error) {
	var buf bytes.Buffer
	if mimeType == "image/jpeg" {
		err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
		return buf.Bytes(), "image/jpeg", err
	}
	err := png.Encode(&buf, img)
	return buf.Bytes(), "image/png", err
}

func attachmentReadText(x store.Attachment, body io.Reader) (*gmcp.CallToolResult, any, error) {
	src, err := io.ReadAll(io.LimitReader(body, attachmentReadTextMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	truncated := len(src) > attachmentReadTextMaxBytes
	text := src
	if truncated {
		text = trimToUTF8Boundary(src[:attachmentReadTextMaxBytes])
	}
	if !utf8.Valid(text) {
		return attachmentReadRefusal(x, "its bytes are not valid UTF-8 text")
	}
	if !truncated {
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(text)}}}, nil, nil
	}
	size, exact := attachmentTextFullSize(x.SizeBytes, body, len(src))
	note := fmt.Sprintf("\n\n[attachment_read truncated this text: it showed the first %d bytes of a file of at least %d bytes. Call attachment_get_url with sha256 %q for the whole file.]", len(text), size, x.SHA256)
	if exact {
		note = fmt.Sprintf("\n\n[attachment_read truncated this text: it showed the first %d of %d bytes. Call attachment_get_url with sha256 %q for the whole file.]", len(text), size, x.SHA256)
	}
	return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(text) + note}}}, nil, nil
}

// attachmentTextFullSize is the file's size for the truncation line: the
// stored row's size_bytes when the transport carries it, otherwise the rest of
// the stream counted up to the probe limit. exact is false only when the count
// reached that limit, so the line can call the number a lower bound.
func attachmentTextFullSize(known int64, rest io.Reader, read int) (size int64, exact bool) {
	if known > 0 {
		return max(known, int64(read)), true
	}
	n, err := io.Copy(io.Discard, io.LimitReader(rest, attachmentReadTextProbeMaxBytes+2))
	total := int64(read) + n
	return total, err == nil && n < attachmentReadTextProbeMaxBytes+2
}

// trimToUTF8Boundary drops a partial rune at the end of b, so a cut never
// splits a multi-byte character.
func trimToUTF8Boundary(b []byte) []byte {
	for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}
