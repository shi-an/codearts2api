package server

import (
	"encoding/json"
	"log"

	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// logRejectedParams 只记录模型和参数形状，不保存消息正文、工具定义或鉴权信息。
func logRejectedParams(acct *pool.Account, model string, messages []upstream.ChatMessage, opts upstream.ChatOptions) {
	log.Printf("upstream params rejected account=%s request_shape=%s", acct.Name, rejectedParamsShape(acct.UID, model, messages, opts))
}

func rejectedParamsShape(accountID, model string, messages []upstream.ChatMessage, opts upstream.ChatOptions) string {
	shape := struct {
		RequestedModel string         `json:"requested_model"`
		ResolvedModel  string         `json:"resolved_model"`
		Channel        string         `json:"channel"`
		Reasoning      string         `json:"reasoning_effort,omitempty"`
		MaxTokens      *int           `json:"max_tokens,omitempty"`
		Temperature    *float64       `json:"temperature,omitempty"`
		TopP           *float64       `json:"top_p,omitempty"`
		ModelMaxTokens int64          `json:"model_max_tokens,omitempty"`
		Tools          int            `json:"tools"`
		ToolChoice     string         `json:"tool_choice,omitempty"`
		Messages       int            `json:"messages"`
		Roles          map[string]int `json:"roles"`
		ToolCalls      int            `json:"tool_calls"`
	}{
		RequestedModel: model, ResolvedModel: upstream.CanonicalModelForAccount(accountID, model),
		Channel:   upstream.AccountModelChannel(accountID, model),
		MaxTokens: opts.MaxTokens, Temperature: opts.Temperature, TopP: opts.TopP,
		Tools: len(opts.Tools), Messages: len(messages), Roles: map[string]int{},
	}
	switch opts.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "auto":
		shape.Reasoning = opts.ReasoningEffort
	default:
		shape.Reasoning = "[other]"
	}
	switch opts.ToolChoice {
	case "auto", "none", "required":
		shape.ToolChoice, _ = opts.ToolChoice.(string)
	}
	for _, msg := range messages {
		role := msg.Role
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			role = "other"
		}
		shape.Roles[role]++
		shape.ToolCalls += len(msg.ToolCalls)
	}
	if models, ok := upstream.AccountModels(accountID); ok {
		for _, info := range models {
			if info.ID == shape.ResolvedModel {
				shape.ModelMaxTokens = info.MaxTokens
				break
			}
		}
	}
	raw, _ := json.Marshal(shape)
	return string(raw)
}
