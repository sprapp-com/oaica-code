package openai

// round50_image_door_contract_test.go — the set of image types this door can
// carry in a data URL, stated in one place because another leg is written
// against it.
//
// The client proxy (cmd/launch.imageDataURL) builds the data URLs this handler
// decodes. It sniffs the payload for a MIME type, and its sniffer knows one
// format this door does not take — GIF — so a client that sent a GIF was
// answered 400 "invalid image input" by its own upstream, while the local leg
// served the same body (2026-09-27 audit, round 50, A50-4). The proxy now
// re-encodes what this door cannot take; these are the types it may claim.

import (
	"encoding/base64"
	"testing"
)

// TestTheImageTypesThisDoorCarries is the contract. If a type is added here the
// proxy may emit it; if one is removed, cmd/launch's imageDataURL has to stop
// emitting it, which is what its own test pins.
func TestTheImageTypesThisDoorCarries(t *testing.T) {
	// A trivially decodable payload per type; only the prefix is under test.
	payloads := map[string]string{
		"jpeg": "/9j/4AAQSkZJRg==",
		"jpg":  "/9j/4AAQSkZJRg==",
		"png":  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg==",
		"webp": "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA",
	}
	for typ, data := range payloads {
		url := "data:image/" + typ + ";base64," + data
		img, err := decodeImageURL(url)
		if err != nil {
			t.Errorf("image/%s was refused: %v — the client proxy is written against this set", typ, err)
			continue
		}
		want, _ := base64.StdEncoding.DecodeString(data)
		if len(img) != len(want) {
			t.Errorf("image/%s decoded to %d bytes, want %d", typ, len(img), len(want))
		}
	}

	for _, other := range []string{"gif", "avif", "tiff", "bmp", "svg+xml"} {
		_, err := decodeImageURL("data:image/" + other + ";base64," + payloads["png"])
		if err == nil {
			t.Errorf("image/%s is accepted here; the client proxy re-encodes anything outside jpeg/jpg/png/webp because this door refuses it", other)
		}
	}
}
