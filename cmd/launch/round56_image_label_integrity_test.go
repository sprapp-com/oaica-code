package launch

// round56_image_label_integrity_test.go — round 56's cross-leg finding on the
// label an image's bytes are written with (F56-5), the client-proxy side of the
// pin in anthropic/round56_run_and_image_label_integrity_test.go.
//
// This leg builds an OpenAI data URL from the converter's api.ImageData, which
// is bytes with no media type: the client's stated type was dropped before this
// leg ever saw it, so the label here is invented from the bytes. Anything the
// sniff does not know is called a jpeg, and a GIF is re-encoded as a PNG (the
// backends this proxy fronts do not all take a GIF part). The metered gateway
// leg, which still holds the client's stated type, prefers it for exactly the
// payloads the sniff cannot name (pinned there in round 39, C-F11).
//
// REJECTED as a finding, not fixed (2026-09-28 audit, round 56): the divergence
// is not a translation of one field into two values but two wires with
// different information in hand — this one has no label to translate. Forcing
// the gateway down to this leg's blanket jpeg would hand a backend a WRONG type
// for a picture the client labelled correctly; teaching this leg the client's
// label would mean carrying it beside api.ImageData, which is the fork's public
// ollama wire shape and has no such field.

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// r56OnePixelGIF is a real 1x1 GIF, so the re-encode has something to decode.
var r56OnePixelGIF = func() []byte {
	b, err := base64.StdEncoding.DecodeString("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")
	if err != nil {
		panic(err)
	}
	return b
}()

func TestTheProxyLabelsAnImageByItsBytes(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("rest of a png")...)
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte("rest")...)
	unknown := []byte("not an image at all, honestly")

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"png bytes", png, "data:image/png;base64,"},
		{"webp bytes", webp, "data:image/webp;base64,"},
		{"a gif is re-encoded as a png", r56OnePixelGIF, "data:image/png;base64,"},
		{"bytes no sniffer knows are a jpeg", unknown, "data:image/jpeg;base64,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := imageDataURL(api.ImageData(tc.data))
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("imageDataURL(%s) = %.40q, want the %s prefix\n"+
					"this wire has no label of the client's to translate — the bytes are all it holds — so its label is sniffed, and the metered gateway leg prefers the client's own stated type where the sniff cannot name one (round 56, F56-5, rejected)", tc.name, got, tc.want)
			}
			if tc.name == "a gif is re-encoded as a png" && len(got) <= len(tc.want)+len(base64.StdEncoding.EncodeToString(tc.data)) {
				t.Errorf("the gif was forwarded as-is (%d bytes of data URL): the point of this case is the re-encode", len(got))
			}
		})
	}
}
