package launch

import "testing"

// remoteDisplayID must keep aggregator "vendor/model" ids whole (OpenRouter
// rejects the stripped form as ambiguous) while still reducing llama-server's
// absolute GGUF file-path ids to a bare basename. See remoteDisplayID doc.
func TestRemoteDisplayID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "openrouter vendor/model id is preserved whole",
			id:   "deepseek/deepseek-chat",
			want: "deepseek/deepseek-chat",
		},
		{
			name: "openrouter stealth id is preserved whole",
			id:   "stealth/ox-alpha",
			want: "stealth/ox-alpha",
		},
		{
			name: "opencode zen flat id untouched",
			id:   "minimax-m3",
			want: "minimax-m3",
		},
		{
			name: "llama-server absolute gguf path reduces to basename sans suffix",
			id:   "/dev/shm/oaica_malay35b_plain_q4km.gguf",
			want: "oaica_malay35b_plain_q4km",
		},
		{
			name: "vllm local model id untouched",
			id:   "kat-awq",
			want: "kat-awq",
		},
		{
			name: "relative .gguf name only drops suffix",
			id:   "model.gguf",
			want: "model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remoteDisplayID(tt.id); got != tt.want {
				t.Fatalf("remoteDisplayID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}
