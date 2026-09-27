package launch

// round51_url_image_refusal_test.go — round 51's finding on the client-side
// proxy leg.
//
// P1: an image the client stated as a URL (`source.type:"url"`, or any image
// part whose data is url TEXT) was forwarded to the upstream as its own
// address string. The upstream is oaica's own OpenAI door, and that door
// refuses a non-data URL with 400 "image URLs are not currently supported" —
// so the client was answered 502, a 5xx, which every SDK retries, for a body
// the local leg's /v1/messages handler refuses in words with a 400
// (middleware/anthropic.go, round 41, C41-13). One client body, two verdicts,
// and this leg's was neither true nor final: the turn could never succeed and
// the client could not tell it from an upstream outage.
//
// The verdict is the local leg's, word for word: the model on this leg is given
// image BYTES, a URL is not one, and the client is told to send a base64 source
// instead (2026-09-27 audit, round 51).

import (
	"net/http"
	"strings"
	"testing"
)

// TestAnImageByURLIsRefusedInWords is P1. The refusal is a 400 the client can
// act on, and no request reaches the upstream: a 502 for this body is a failure
// the client cannot tell from an outage, and one it is told to retry.
func TestAnImageByURLIsRefusedInWords(t *testing.T) {
	const want = `cannot be represented on this leg`
	for _, c := range []struct {
		name  string
		block string
		// want is the text the client must be able to read; the local leg's
		// wording for the url case, and the decode's own for a base64 part that
		// is not base64 (which is refused before this check, also in words).
		want string
	}{
		{
			name:  "url-source",
			block: `{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}`,
			want:  want,
		},
		{
			name:  "protocol-relative",
			block: `{"type":"image","source":{"type":"url","url":"//example.com/cat.png"}}`,
			want:  want,
		},
		{
			name:  "base64-holding-a-url",
			block: `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"https://example.com/cat.png"}}`,
			want:  "invalid base64 image data",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var captured string
			up := round49CapturingUpstream(t, &captured)
			proxy := startCalibProxy(t, up.URL, "r51-"+c.name)

			body := `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":[` + c.block + `]}]}`
			status, out := round49Post(t, proxy, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, body:\n%s\nthe door refuses this URL with 400 and this leg reported it to the client as a retryable 5xx", status, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("the refusal does not say what happened (%q):\n%s\nthe local leg refuses the same body in these words", c.want, out)
			}
			if captured != "" {
				t.Errorf("a body the leg cannot serve reached the upstream anyway:\n%s", captured)
			}
		})
	}
}

// TestAnImageByBase64IsStillServed is the other side of P1: bytes are what this
// leg can carry, and a real image part still reaches the upstream as a data
// URL. Without this, "refuse more" would pass the test above.
func TestAnImageByBase64IsStillServed(t *testing.T) {
	const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

	var captured string
	up := round49CapturingUpstream(t, &captured)
	proxy := startCalibProxy(t, up.URL, "r51-b64")

	body := `{"model":"kat-awq","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png1x1 + `"}}]}]}`
	status, out := round49Post(t, proxy, body)
	if status != http.StatusOK {
		t.Fatalf("a base64 image was refused with %d:\n%s\nbytes are what this leg is for", status, out)
	}
	if !strings.Contains(captured, "data:image/png;base64,") {
		t.Errorf("the image did not reach the upstream as image bytes:\n%s", captured)
	}
}
