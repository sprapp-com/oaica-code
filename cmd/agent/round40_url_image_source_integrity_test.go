package agent

// round40_url_image_source_integrity_test.go — C40-2. api.ImageData carries a
// remote image as its own URL text (anthropic.resolveImageSource), and the
// shim base64-encoded that text into a data URL: the upstream was sent a
// picture of a string — and since round 39 the converter refuses such a payload
// outright, so the shim turned a working url image into a 400 whose cause the
// user could not see.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func TestAURLImageReachesTheWireAsItsSource(t *testing.T) {
	const url = "https://example.test/shot.png"
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{{
			Role:    "user",
			Content: "what is this",
			Images:  []api.ImageData{api.ImageData(url)},
		}},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("Messages = %d, want 1", len(out.Messages))
	}

	raw, err := json.Marshal(out.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)

	if !strings.Contains(wire, `"type":"url"`) || !strings.Contains(wire, url) {
		t.Errorf("the image did not reach the wire as a url source: %s\nthe wire takes the source as it stands, and base64-encoding a URL sends the upstream a picture of the address text", wire)
	}
	if encoded := base64.StdEncoding.EncodeToString([]byte(url)); strings.Contains(wire, encoded) {
		t.Errorf("the url was base64-encoded into the block: %s\nthe converter refuses a base64 payload that decodes to a URL, so this is a 400 the user cannot see the cause of", wire)
	}
}

// TestARealImageStillTravelsAsBase64 guards the other half: the url branch must
// not swallow the bytes a client actually sent.
func TestARealImageStillTravelsAsBase64(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{{
			Role:    "user",
			Content: "what is this",
			Images:  []api.ImageData{api.ImageData(png)},
		}},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	raw, _ := json.Marshal(out.Messages[0].Content)
	wire := string(raw)

	want := base64.StdEncoding.EncodeToString(png)
	if !strings.Contains(wire, want) || !strings.Contains(wire, `"type":"base64"`) {
		t.Errorf("a real image did not travel as its base64 bytes: %s", wire)
	}
}
