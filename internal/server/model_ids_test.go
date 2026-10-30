package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

func TestChatAndProbeUseAccountInferenceIDs(t *testing.T) {
	for _, mode := range []string{"chat", "probe"} {
		for _, tc := range []struct {
			name, input, wantID, channel string
			discovered                   bool
		}{
			{name: "cold_glm53", input: "GLM-5.3", wantID: "glm-5.3"},
			{name: "display_alias", input: "Server Display Model", wantID: "wire-server-model", channel: upstream.MaasEducation, discovered: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				uid := "model-id-" + t.Name()
				if tc.discovered {
					upstream.SetAccountModels(uid, []upstream.ModelInfo{{ID: tc.wantID, Name: tc.input, MaasType: tc.channel}})
					// 模拟其他账号最后刷新全局索引，但实际发送必须保留本账号的 ID。
					upstream.SetAccountModels(uid+"-other", []upstream.ModelInfo{{ID: strings.ToUpper(tc.wantID), Name: tc.input}})
				}
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var body struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.Model != tc.wantID || r.Header.Get(upstream.HeaderMaasType) != tc.channel {
						t.Errorf("wrong inference request: model=%q channel=%q, want %q/%q", body.Model, r.Header.Get(upstream.HeaderMaasType), tc.wantID, tc.channel)
					}
					if signed := strings.Contains(r.Header.Get("Authorization"), upstream.HeaderMaasType); signed != (tc.channel != "") {
						t.Errorf("channel signature=%v, channel=%q", signed, tc.channel)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer srv.Close()
				a := auth.New(uid, "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
				p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
				if err != nil {
					t.Fatal(err)
				}
				acct := p.Accounts()[0]
				acct.Client = upstream.NewWithChatEndpoint(time.Second, srv.URL)
				h := NewHandler(Config{Pool: p, MaxRotate: 1})
				if mode == "probe" {
					h.probeModel(acct, tc.input)
				} else {
					body, err := json.Marshal(map[string]any{"model": tc.input, "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
						t.Fatalf("chat failed: status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
				if got := requests.Load(); got != 1 {
					t.Errorf("upstream requests=%d, want 1", got)
				}
				for _, model := range []string{tc.input, tc.wantID} {
					if st, _ := modelAvailability(uid, model); st != availUsable {
						t.Errorf("availability not shared by alias and ID: model=%q state=%v", model, st)
					}
				}
			})
		}
	}
}
