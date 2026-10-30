package server

import (
	"encoding/json"
	"strings"
	"testing"

	"codearts2api/internal/upstream"
)

func TestRejectedParamsShapeKeepsUsefulFieldsWithoutConversation(t *testing.T) {
	const secret = "private-message-or-tool-argument"
	upstream.SetAccountModels("diagnostic-account", []upstream.ModelInfo{{ID: "deepseek-v4.1-flash-1017", Name: "Diagnostic DeepSeek", MaxTokens: 8192, OutputContextWindow: 393216, ContextWindow: 1048576}})
	max := 32768
	shape := rejectedParamsShape("diagnostic-account", "Diagnostic DeepSeek", []upstream.ChatMessage{
		{Role: "user", Content: secret},
		{Role: "assistant", Content: secret, ReasoningContent: func() *string { s := secret; return &s }(), ToolCalls: []upstream.ChatToolCall{{Function: upstream.ChatToolFunction{Name: secret, Arguments: secret}}}},
	}, upstream.ChatOptions{MaxTokens: &max, ReasoningEffort: "high", ToolChoice: "auto", Tools: []map[string]any{{"description": secret}}})
	if strings.Contains(shape, secret) {
		t.Fatalf("diagnostics contained conversation data: %s", shape)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(shape), &fields); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"requested_model": "Diagnostic DeepSeek", "resolved_model": "deepseek-v4.1-flash-1017",
		"max_tokens": float64(max), "model_max_tokens": float64(8192), "reasoning_effort": "high",
		"model_context_window":        float64(1048576),
		"model_output_context_window": float64(393216), "model_output_limit": float64(393216),
		"model_output_limit_source": "output_context_window",
		"tools":                     float64(1), "messages": float64(2), "tool_calls": float64(1),
	} {
		if fields[key] != want {
			t.Errorf("diagnostic %s=%v, want %v", key, fields[key], want)
		}
	}
}
