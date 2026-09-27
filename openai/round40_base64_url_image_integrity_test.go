package openai

// round40_base64_url_image_integrity_test.go — C40-5. A payload that says
// "base64" and decodes to a URL is not an image: carried on as bytes it reached
// the runner as a JPEG of the address text (llm/llama_server.go labels an
// unrecognised payload "image/jpeg"), so the client was told its image was
// understood while the model was asked about a picture of a string. Both other
// legs refuse this payload in words; this is the /v1/chat/completions door onto
// the same rule.

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestABase64PayloadThatDecodesToAURLIsRefused(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{
			name: "https",
			url:  "https://example.test/a.png",
		},
		{
			name: "scheme-absolute",
			url:  "file:///tmp/a.png",
		},
		{
			name: "data uri",
			url:  "data:image/png;base64,aGVsbG8=",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			payload := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(tt.url))
			img, err := decodeImageURL(payload)
			if err == nil {
				t.Fatalf("decodeImageURL accepted a base64 payload that decodes to %q and returned %d bytes: the bytes are the address text, and the runner labels them image/jpeg — the client is told its image was understood while the model is asked about a picture of a string", tt.url, len(img))
			}
			if !strings.Contains(err.Error(), "URL") {
				t.Errorf("the refusal does not name what it refused: %v", err)
			}
		})
	}
}

// TestRealImageBytesAreStillAccepted guards the other half: the rule must
// refuse a payload that IS a URL, not one that merely contains url-like bytes.
func TestRealImageBytesAreStillAccepted(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0, 0x49, 0x45, 0x4e, 0x44}
	// PNG bytes with an https link inside the metadata: still an image.
	withLink := append(append([]byte{}, png...), []byte("https://example.test")...)

	for _, tt := range []struct {
		name string
		data []byte
	}{
		{name: "plain png", data: png},
		{name: "png carrying a link in its bytes", data: withLink},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tt.data)
			img, err := decodeImageURL(payload)
			if err != nil {
				t.Fatalf("decodeImageURL refused real image bytes: %v", err)
			}
			if len(img) != len(tt.data) {
				t.Errorf("decoded %d bytes, want %d", len(img), len(tt.data))
			}
		})
	}
}
