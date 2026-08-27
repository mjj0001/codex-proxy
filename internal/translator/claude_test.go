package translator

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToOpenAIMapsClaudeThinkingEffort(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantEffort string
		wantStream bool
	}{
		{
			name:       "thinking budget",
			body:       `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":24576}}`,
			wantEffort: "high",
			wantStream: true,
		},
		{
			name:       "disabled thinking wins",
			body:       `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`,
			wantEffort: "none",
			wantStream: false,
		},
		{
			name:       "output config fallback",
			body:       `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high"}}`,
			wantEffort: "high",
			wantStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, model, stream := ConvertClaudeRequestToOpenAI([]byte(tt.body))
			if model != "claude-opus-5" {
				t.Fatalf("model = %q, want %q", model, "claude-opus-5")
			}
			if stream != tt.wantStream {
				t.Fatalf("stream = %v, want %v", stream, tt.wantStream)
			}
			if got := gjson.GetBytes(out, "reasoning.effort").String(); got != tt.wantEffort {
				t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, tt.wantEffort, out)
			}
		})
	}
}
