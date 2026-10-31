package llm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Image is a picture attached to a user turn, base64 encoded (no data: prefix).
type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// DataURL renders the image as a data: URL (OpenAI-compatible APIs).
func (i Image) DataURL() string { return "data:" + i.MediaType + ";base64," + i.Data }

// Limits for images accepted from callers. Every provider we speak takes
// these four types; the size cap keeps a request well below what the APIs
// accept (Anthropic 5 MB, Mistral 10 MB per image).
const (
	MaxImages     = 4
	MaxImageBytes = 5 << 20 // decoded
)

var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

// ErrImage marks an image the caller sent that cannot be used.
var ErrImage = errors.New("invalid image")

// ParseImage accepts a data: URL or plain base64 and checks type and size.
// The type is sniffed from the bytes; a declared mediaType that disagrees is
// rejected rather than trusted.
func ParseImage(s, mediaType string) (Image, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		head, data, ok := strings.Cut(s[len("data:"):], ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return Image{}, fmt.Errorf("%w: data URL must be base64 encoded", ErrImage)
		}
		mediaType, s = strings.TrimSuffix(head, ";base64"), data
	} else if strings.Contains(s, "://") {
		return Image{}, fmt.Errorf("%w: send the image itself (base64 or a data: URL), not a link", ErrImage)
	}
	if base64.StdEncoding.DecodedLen(len(s)) > MaxImageBytes+3 {
		return Image{}, fmt.Errorf("%w: larger than %d MB", ErrImage, MaxImageBytes>>20)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Image{}, fmt.Errorf("%w: not valid base64", ErrImage)
	}
	if len(raw) > MaxImageBytes {
		return Image{}, fmt.Errorf("%w: larger than %d MB", ErrImage, MaxImageBytes>>20)
	}
	sniffed := http.DetectContentType(raw)
	if !imageTypes[sniffed] {
		return Image{}, fmt.Errorf("%w: only JPEG, PNG, WebP and GIF are supported", ErrImage)
	}
	if mediaType != "" && !strings.EqualFold(mediaType, sniffed) {
		return Image{}, fmt.Errorf("%w: declared %s but the data is %s", ErrImage, mediaType, sniffed)
	}
	return Image{MediaType: sniffed, Data: s}, nil
}

// WithoutImages returns the history with every image replaced by a short
// note, for storage: a session must not grow by megabytes per picture, and
// re-sending old pictures on every later turn would bill them again. The
// model keeps what it said about them.
func WithoutImages(h []Message) []Message {
	var out []Message
	for i, m := range h {
		if len(m.Images) == 0 {
			continue
		}
		if out == nil {
			out = append([]Message(nil), h...)
		}
		note := fmt.Sprintf("[%d image(s) were attached here; not kept in the history]", len(m.Images))
		if m.Content != "" {
			note = m.Content + "\n" + note
		}
		out[i].Content, out[i].Images = note, nil
	}
	if out == nil {
		return h
	}
	return out
}
