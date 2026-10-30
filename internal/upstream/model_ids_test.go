package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

// 26.10.100 客户端的展示名与推理 ID 可以不同，推理请求必须使用参数里的 ID。
func TestDiscoveredWireIDsOverrideDisplayNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpAgentList:
			io.WriteString(w, `{"agents":[{"agent_id":"a1","is_primary_agent":true}]}`)
		case EpAgentDetail:
			io.WriteString(w, `{"gpts":{"models":[
				{"model_name":"GLM-5.3","model_alias":"legacy-glm-alias","model_id":"legacy-glm-id","model_parameters":{"model_id":"glm-5.3","context_window":200000}},
				{"model_name":"GLM-5.2-ArkTS-SPARK","model_alias":"glm-5.2-sft-harmony","model_parameters":{"model_id":"glm-5.2-sft-harmony"}},
				{"model_name":"OpenPangu-2.0-Pro","model_parameters":{"model_id":"openpangu-2.0-pro"}},
				{"model_name":"Display-Only-Name","model_alias":"wire-alias-only"}
			]}}`)
		case EpModelBuiltin:
			io.WriteString(w, `{"builtinModels":[
				{"model_id":"glm-5.3","model_name":"GLM-5.3"},
				{"model_id":"glm-5.2-sft-harmony","model_name":"GLM-5.2-ArkTS-SPARK"},
				{"model_id":"openpangu-2.0-pro","model_name":"OpenPangu-2.0-Pro"}
			]}`)
		case EpBenefitGateway, EpEducationGateway:
			io.WriteString(w, `{"enabled":false}`)
		case EpChatV2:
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Model != "glm-5.3" {
				t.Errorf("display name leaked to chat request: model=%q, want glm-5.3", body.Model)
			}
			io.WriteString(w, "data: {}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	acct := &auth.Auth{UserID: "wire-id-discovery", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	infos, err := c.FetchModels(acct)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	for _, pair := range [][2]string{
		{"GLM-5.3", "glm-5.3"},
		{"GLM-5.2-ArkTS-SPARK", "glm-5.2-sft-harmony"},
		{"OpenPangu-2.0-Pro", "openpangu-2.0-pro"},
		{"Display-Only-Name", "wire-alias-only"},
	} {
		if got := CanonicalModel(pair[0]); got != pair[1] {
			t.Errorf("CanonicalModel(%q)=%q, want %q", pair[0], got, pair[1])
		}
		if mi, ok := byID[pair[1]]; !ok || mi.Name != pair[0] {
			t.Errorf("missing wire/display identity %v: %+v", pair, mi)
		}
	}
	if len(infos) != 4 {
		t.Errorf("display names created duplicate models: %+v", infos)
	}
	rc, err := c.ChatStreamWithOptions(context.Background(), "chat", []ChatMessage{{Role: "user", Content: "hi"}}, "trace", SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}, "", "glm-5.3", ChatOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
}

func TestChatStreamKeepsAccountModelIdentity(t *testing.T) {
	const displayName = "Account Model Display"
	SetAccountModels("wire-account-a", []ModelInfo{{ID: "wire-account-model", Name: displayName, MaasType: MaasEducation}})
	SetAccountModels("wire-account-b", []ModelInfo{{ID: "WIRE-ACCOUNT-MODEL", Name: displayName}})
	// B 最后刷新，不能改写 A 的推理 ID 或渠道。
	if got := CanonicalModel(displayName); got != "WIRE-ACCOUNT-MODEL" {
		t.Fatalf("test must exercise a conflicting global mapping, got %q", got)
	}
	for _, tc := range []struct {
		account, input, wantID, wantChannel string
	}{
		{"wire-account-a", displayName, "wire-account-model", MaasEducation},
		{"wire-account-a", "WIRE-ACCOUNT-MODEL", "wire-account-model", MaasEducation},
		{"wire-account-b", displayName, "WIRE-ACCOUNT-MODEL", ""},
	} {
		t.Run(tc.account+"/"+tc.input, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != tc.wantID || r.Header.Get(HeaderMaasType) != tc.wantChannel {
					t.Errorf("wrong account mapping: model=%q channel=%q, want %q/%q", body.Model, r.Header.Get(HeaderMaasType), tc.wantID, tc.wantChannel)
				}
				if signed := strings.Contains(r.Header.Get("Authorization"), HeaderMaasType); signed != (tc.wantChannel != "") {
					t.Errorf("channel signature=%v, channel=%q", signed, tc.wantChannel)
				}
				io.WriteString(w, "data: {}\n\n")
			}))
			defer srv.Close()
			c := NewWithChatEndpoint(time.Second, srv.URL)
			cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}
			rc, err := c.ChatStreamForAccount(context.Background(), tc.account, "chat", []ChatMessage{{Role: "user", Content: "hi"}}, "trace", cred, tc.input, ChatOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rc.Close()
		})
	}
}

func TestDisplayAliasCannotOverrideActualModelID(t *testing.T) {
	SetAccountModels("wire-name-collision", []ModelInfo{
		{ID: "actual-model", Name: "Other Display Name"},
		{ID: "other-model", Name: "Actual-Model", MaasType: MaasBenefit},
	})
	if got := CanonicalModelForAccount("wire-name-collision", "Actual-Model"); got != "actual-model" {
		t.Errorf("display alias overrode actual model ID: %q", got)
	}
	if got := AccountModelChannel("wire-name-collision", "Actual-Model"); got != "" {
		t.Errorf("display alias changed ordinary model route: %q", got)
	}
}

func TestMergeModelsKeepsPrioritySourceIDAndDisplayName(t *testing.T) {
	got := MergeModels(
		[]ModelInfo{{ID: "wire-priority-model", Name: "Priority Display"}},
		[]ModelInfo{{ID: "WIRE-PRIORITY-MODEL", Name: "WIRE-PRIORITY-MODEL", ContextWindow: 8192, MaxTokens: 1024}},
	)
	if len(got) != 1 || got[0].ID != "wire-priority-model" || got[0].Name != "Priority Display" || got[0].ContextWindow != 8192 || got[0].MaxTokens != 1024 {
		t.Fatalf("metadata enrichment changed inference identity: %+v", got)
	}
}

func TestLegacyAliasUsesAccountInferenceID(t *testing.T) {
	SetAccountModels("legacy-wire-lower", []ModelInfo{{ID: "glm-5.2"}})
	SetAccountModels("legacy-wire-upper", []ModelInfo{{ID: "GLM-5.2"}})
	if got := CanonicalModelForAccount("legacy-wire-lower", "snap-chat"); got != "glm-5.2" {
		t.Fatalf("legacy alias bypassed account ID: %q", got)
	}
}
