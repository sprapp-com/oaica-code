package launch

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriteRefreshedModelSources_SectionsInPickerOrder(t *testing.T) {
	var buf bytes.Buffer
	WriteRefreshedModelSources(&buf, RefreshedModelSources{
		Local:  []string{"qwen2.5:7b"},
		Remote: []string{"opencode-go/glm-5.3"},
		Router: []string{"oaica-35b-a3b-vision"},
	})
	out := buf.String()
	localIdx := strings.Index(out, "Local")
	remoteIdx := strings.Index(out, "Remote")
	routerIdx := strings.Index(out, "Router")
	if localIdx == -1 || remoteIdx == -1 || routerIdx == -1 {
		t.Fatalf("expected all 3 section headers, got: %q", out)
	}
	if !(localIdx < remoteIdx && remoteIdx < routerIdx) {
		t.Errorf("expected order Local < Remote < Router, got Local=%d Remote=%d Router=%d", localIdx, remoteIdx, routerIdx)
	}
	if !strings.Contains(out, "qwen2.5:7b") || !strings.Contains(out, "opencode-go/glm-5.3") || !strings.Contains(out, "oaica-35b-a3b-vision") {
		t.Errorf("expected all model names present, got: %q", out)
	}
}

func TestWriteRefreshedModelSources_EmptyShowsMessage(t *testing.T) {
	var buf bytes.Buffer
	WriteRefreshedModelSources(&buf, RefreshedModelSources{})
	out := buf.String()
	if !strings.Contains(out, "No models discovered") {
		t.Errorf("expected 'No models discovered' message for empty result, got: %q", out)
	}
}

func TestWriteRefreshedModelSources_ShowsWarningsForErrs(t *testing.T) {
	var buf bytes.Buffer
	WriteRefreshedModelSources(&buf, RefreshedModelSources{
		Local: []string{"a"},
		Errs:  []error{errors.New("router unreachable")},
	})
	out := buf.String()
	if !strings.Contains(out, "warning") || !strings.Contains(out, "router unreachable") {
		t.Errorf("expected a warning line for the error, got: %q", out)
	}
}
