package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

type testModelCatalog struct {
	ensure func(context.Context, *auth.Auth) error
}

func (c testModelCatalog) EnsureAccountModels(ctx context.Context, acct *auth.Auth) error {
	return c.ensure(ctx, acct)
}

func (c testModelCatalog) FetchModels(acct *auth.Auth) ([]upstream.ModelInfo, error) {
	if err := c.ensure(context.Background(), acct); err != nil {
		return nil, err
	}
	infos, _ := upstream.AccountModels(acct.UserID)
	return infos, nil
}

func TestChatCompletionsCapsDiscoveredOutputWithoutDroppingToolsOrHistory(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(field, func(t *testing.T) {
			var chats, discoveries atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chats.Add(1)
				var body struct {
					Model      string           `json:"model"`
					MaxTokens  int              `json:"max_tokens"`
					Messages   []map[string]any `json:"messages"`
					Tools      []map[string]any `json:"tools"`
					ToolChoice string           `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				// 隔离的模拟上游：只验证目录预算策略，不代表真实服务端阈值。
				if body.MaxTokens > 65536 {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, "InferHub.001001005.400 The request param is invalid")
					return
				}
				if body.Model != "deepseek-v4.1-flash-1017" || body.MaxTokens != 65536 ||
					len(body.Messages) != 155 || len(body.Tools) != 86 || body.ToolChoice != "auto" {
					t.Errorf("unexpected normalized request: model=%s max=%d messages=%d tools=%d choice=%s",
						body.Model, body.MaxTokens, len(body.Messages), len(body.Tools), body.ToolChoice)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer backend.Close()
			h, _ := queueTestHandler(t, backend.URL)
			h.cfg.Upstream = testModelCatalog{ensure: func(ctx context.Context, acct *auth.Auth) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("chat model discovery has no deadline")
				}
				discoveries.Add(1)
				upstream.SetAccountModels(acct.UserID, []upstream.ModelInfo{{ID: "deepseek-v4.1-flash-1017", MaxTokens: 65536, ContextWindow: 1048576}})
				return nil
			}}
			messages := []map[string]any{{"role": "system", "content": "synthetic limit test"}}
			for i := 0; i < 126; i++ {
				messages = append(messages, map[string]any{"role": "user", "content": "synthetic history"})
				if i < 28 {
					messages = append(messages, map[string]any{"role": "assistant", "content": "synthetic response"})
				}
			}
			var tools []map[string]any
			for i := 0; i < 86; i++ {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
					"name": fmt.Sprintf("test_tool_%d", i), "parameters": map[string]any{"type": "object"},
				}})
			}
			payload, err := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash-1017", field: 214057,
				"messages": messages, "tools": tools, "tool_choice": "auto"})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload))))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) || chats.Load() != 1 || discoveries.Load() != 1 {
				t.Fatalf("limit normalization failed: status=%d chats=%d discoveries=%d body=%s", rec.Code, chats.Load(), discoveries.Load(), rec.Body.String())
			}
		})
	}
}

func TestCanceledModelDiscoveryReleasesAccountWithoutChat(t *testing.T) {
	var chats atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chats.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()
	h, p := queueTestHandler(t, backend.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cfg.Upstream = testModelCatalog{ensure: func(lookupCtx context.Context, acct *auth.Auth) error {
		cancel()
		<-lookupCtx.Done()
		return lookupCtx.Err()
	}}
	payload := `{"max_tokens":214057,"messages":[{"role":"user","content":"synthetic test"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)).WithContext(ctx))
	if chats.Load() != 0 || p.List()[0]["active_concurrent"] != 0 || p.List()[0]["err_count"] != 0 {
		t.Fatalf("canceled discovery retained lock, sent chat or penalized account: chats=%d state=%v", chats.Load(), p.List())
	}
}

func TestChatCompletionsRecalculatesOutputLimitAfterAccountRotation(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		n := calls.Add(1)
		if n == 1 {
			if body.MaxTokens != 8192 {
				t.Errorf("first account budget=%d, want 8192", body.MaxTokens)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if body.MaxTokens != 65536 {
			t.Errorf("rotated account inherited prior budget: %d, want 65536", body.MaxTokens)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	h, p := queueTestHandler(t, backend.URL, backend.URL)
	for i, acct := range p.Accounts() {
		limit := int64(8192)
		if i == 1 {
			limit = 65536
		}
		upstream.SetAccountModels(acct.UID, []upstream.ModelInfo{{ID: "deepseek-v4.1-flash-1017", MaxTokens: limit}})
	}
	payload := `{"model":"deepseek-v4.1-flash-1017","max_tokens":214057,"messages":[{"role":"user","content":"synthetic test"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("account rotation failed: status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestOutputLimitDoesNotInventOrIncreaseBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int64
		requested int
		omit      bool
	}{
		{name: "unknown", requested: 214057},
		{name: "within_limit", requested: 8192, limit: 65536},
		{name: "larger_channel_limit", requested: 214057, limit: 393216},
		{name: "omitted", limit: 65536, omit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := queueTestHandler(t, "http://unused.invalid")
			acct := p.Accounts()[0]
			upstream.SetAccountModels(acct.UID, []upstream.ModelInfo{{ID: "output-limit-model", MaxTokens: tc.limit}})
			upstream.SetAccountModels(acct.UID+"-other", []upstream.ModelInfo{{ID: "output-limit-model", MaxTokens: 1024}})
			requested := &tc.requested
			if tc.omit {
				requested = nil
			}
			got := h.outputTokensForAccount(context.Background(), acct, "output-limit-model", requested)
			if got != requested {
				t.Fatalf("changed valid/unknown/omitted budget: got=%v, want=%v", got, requested)
			}
		})
	}
}

func TestChatUsesSelectedChannelOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		models  []upstream.ModelInfo
		channel string
		wantMax int
	}{
		{
			name: "desktop_larger_than_ide",
			models: []upstream.ModelInfo{
				{ID: "channel-budget-model", Benefit: true, OutputContextWindow: 8192},
				{ID: "channel-budget-model", MaxTokens: 65536, OutputContextWindow: 393216},
			},
			wantMax: 214057,
		},
		{
			name: "benefit_and_education",
			models: []upstream.ModelInfo{
				{ID: "channel-budget-model", MaasType: upstream.MaasEducation, OutputContextWindow: 32768},
				{ID: "channel-budget-model", Benefit: true, MaxTokens: 8192, OutputContextWindow: 131072},
			},
			channel: upstream.MaasBenefit, wantMax: 131072,
		},
		{
			name: "education",
			models: []upstream.ModelInfo{
				{ID: "channel-budget-model", MaasType: upstream.MaasEducation, MaxTokens: 8192, OutputContextWindow: 65536},
			},
			channel: upstream.MaasEducation, wantMax: 65536,
		},
		{
			name: "ordinary_unknown_budget",
			models: []upstream.ModelInfo{
				{ID: "channel-budget-model", Benefit: true, OutputContextWindow: 8192},
				{ID: "channel-budget-model"},
			},
			wantMax: 214057,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chats atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chats.Add(1)
				var body struct {
					Model     string `json:"model"`
					MaxTokens int    `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "channel-budget-model" || body.MaxTokens != tc.wantMax || r.Header.Get(upstream.HeaderMaasType) != tc.channel {
					t.Errorf("request mixed channel/budget: model=%q max=%d channel=%q", body.Model, body.MaxTokens, r.Header.Get(upstream.HeaderMaasType))
				}
				if signed := strings.Contains(r.Header.Get("Authorization"), upstream.HeaderMaasType); signed != (tc.channel != "") {
					t.Errorf("channel signature mismatch: signed=%v channel=%q", signed, tc.channel)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer backend.Close()
			h, p := queueTestHandler(t, backend.URL)
			upstream.SetAccountModels(p.Accounts()[0].UID, tc.models)
			payload := `{"model":"channel-budget-model","max_tokens":214057,"messages":[{"role":"user","content":"synthetic test"}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
			if rec.Code != http.StatusOK || chats.Load() != 1 {
				t.Fatalf("chat failed: status=%d chats=%d body=%s", rec.Code, chats.Load(), rec.Body.String())
			}
		})
	}
}
