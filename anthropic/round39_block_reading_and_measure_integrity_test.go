package anthropic

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestAnUnknownBlockTypeIsNamedNotDropped is C-F5. A block this converter has
// no arm for used to be counted and dropped: the turn was answered 200 with the
// block missing from the prompt and nothing said to the client about it — an
// image-only or unknown-only message left an empty user turn. The gateway leg
// refuses the same body in words, so one request was a 400 on one leg and an
// answer to an altered prompt on the other.
func TestAnUnknownBlockTypeIsNamedNotDropped(t *testing.T) {
	_, err := convertMessage(MessageParam{
		Role:    "user",
		Content: []ContentBlock{{Type: "alien", Text: strPtrR39("something")}},
	})
	if err == nil {
		t.Fatal("a content block type this converter cannot represent must be an error, not a block that silently disappears from the prompt")
	}
	if !strings.Contains(err.Error(), `"alien"`) {
		t.Errorf("the refusal does not name the type it refused: %v\nthe client cannot tell \"you dropped my block\" from a bug without the name", err)
	}
}

// TestAURLSourceIsReadByWhatItIs is A-F1/C-F9. Matching only the three
// lowercase prefixes ("http://", "https://", "data:") re-encoded a url source
// the client wrote as "HTTPS://…" or "ftp://…" into a JPEG OF THE ADDRESS TEXT,
// so the model was asked about a picture of a URL and the backend was never
// sent the image the client pointed at.
func TestAURLSourceIsReadByWhatItIs(t *testing.T) {
	urls := []string{
		"https://example.test/a.png",
		"HTTPS://example.test/a.png",
		"ftp://example.test/a.png",
		"//example.test/a.png",
		"DATA:image/png;base64,AAAA",
		"  https://example.test/a.png  ",
	}
	for _, u := range urls {
		if !IsImageURL(api.ImageData(u)) {
			t.Errorf("IsImageURL(%q) = false: the source is a URL, and reading it as image bytes has the backend decode the address text as a picture", u)
		}
	}
	for _, b := range []string{"", "\x89PNG\r\n\x1a\n", "GIF89a", "RIFF....WEBPVP8 ", "\xff\xd8\xff\xe0"} {
		if IsImageURL(api.ImageData(b)) {
			t.Errorf("IsImageURL(%q) = true: image bytes are not a URL", b)
		}
	}
}

// TestABase64SourceThatDecodesToAURLIsRefused is A-F1's other direction. A
// source that says "base64" and decodes to a URL is not an image: byte-sniffing
// sent it on as a URL the client never named — the picture was dropped and the
// backend was told to fetch an address out of the payload.
func TestABase64SourceThatDecodesToAURLIsRefused(t *testing.T) {
	src := &ImageSource{
		Type:      "base64",
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString([]byte("https://example.test/a.png")),
	}
	if _, err := resolveImageSource(src); err == nil {
		t.Fatal("base64 data that decodes to a URL must be refused: carried on as a URL, the client's picture is dropped and the backend fetches whatever address the payload names")
	}
}

// TestASourceWithNoTypeIsABase64Source is C-F10. "" is what a client spells
// when it writes media_type+data with no type; refusing it 400'd a body the
// gateway leg has always read.
func TestASourceWithNoTypeIsABase64Source(t *testing.T) {
	payload := []byte("\x89PNG\r\n\x1a\nrest")
	src := &ImageSource{
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(payload),
	}
	got, err := resolveImageSource(src)
	if err != nil {
		t.Fatalf("a source with media_type and data but no type must be read as base64: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("decoded %q, want the payload %q", got, payload)
	}
}

// TestANonObjectToolResultElementIsRefusedNotSkipped is C-F6. A non-object
// element was skipped here and json.Marshal'd into the prompt by the gateway
// leg, so the same body reached the model as two different prompts ("after" vs
// "1\n\"x\"\nafter").
func TestANonObjectToolResultElementIsRefusedNotSkipped(t *testing.T) {
	_, _, err := convertToolResultContent([]any{"x"})
	if err == nil {
		t.Fatal("a tool_result content element that is not a JSON object must be refused: skipped on one leg and marshalled into the prompt on the other, the same body becomes two different prompts")
	}
}

// TestANestedURLImageKeepsItsURL is C-F1/A-F2. The nested arm hand-builds an
// ImageSource, and a field it does not copy is a field resolveImageSource
// cannot see — a nested url source was refused with "url, with no url", a body
// the gateway leg forwards to the model.
func TestANestedURLImageKeepsItsURL(t *testing.T) {
	content := []any{
		map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "url", "url": "https://example.test/shot.png"},
		},
	}
	_, images, err := convertToolResultContent(content)
	if err != nil {
		t.Fatalf("a nested url image source must be carried, not refused: %v", err)
	}
	if len(images) != 1 || string(images[0]) != "https://example.test/shot.png" {
		t.Fatalf("images = %v, want the source's URL: the arm copies type/media_type/data and dropped url, so this screenshot reached the model on one leg only", images)
	}
}

func strPtrR39(s string) *string { return &s }
