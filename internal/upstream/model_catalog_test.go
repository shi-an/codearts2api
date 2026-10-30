package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

// 所有数值都是隔离的目录样本，不代表对应渠道的真实服务端限制。
func TestModelCatalogReadsDesktopOutputFieldsFromEverySource(t *testing.T) {
	for _, source := range []string{"agent", "builtin", "combined", MaasBenefit, MaasEducation} {
		t.Run(source, func(t *testing.T) {
			const record = `{"model_id":"catalog-output-model","max_tokens":65536,"output_context_window":393216,"input_context_window":1048576}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpAgentList:
					io.WriteString(w, `{"agents":[{"agent_id":"a1","is_primary_agent":true}]}`)
				case EpAgentDetail:
					if source == "agent" {
						io.WriteString(w, `{"gpts":{"models":[{"model_name":"Catalog Output Model","model_parameters":`+record+`}]}}`)
					} else if source == "combined" {
						io.WriteString(w, `{"gpts":{"models":[{"model_parameters":{"model_id":"catalog-output-model","max_tokens":65536,"context_window":1048576}}]}}`)
					} else {
						io.WriteString(w, `{"gpts":{"models":[]}}`)
					}
				case EpModelBuiltin:
					if source == "builtin" {
						io.WriteString(w, `{"builtinModels":[`+record+`]}`)
					} else if source == "combined" {
						io.WriteString(w, `{"builtinModels":[{"model_id":"catalog-output-model","output_context_window":393216}]}`)
					} else {
						io.WriteString(w, `{"builtinModels":[]}`)
					}
				case EpBenefitGateway, EpEducationGateway:
					if (source == MaasBenefit && r.URL.Path == EpBenefitGateway) || (source == MaasEducation && r.URL.Path == EpEducationGateway) {
						io.WriteString(w, `{"enabled":true}`)
					} else {
						io.WriteString(w, `{"enabled":false}`)
					}
				case EpBenefitConfig:
					io.WriteString(w, `{"error_code":"0000","result":{"models":[`+record+`]}}`)
				default:
					t.Errorf("unexpected catalog request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase, c.benefitBase = srv.URL, srv.URL
			c.SetBenefitAutoClaim(false)
			acct := &auth.Auth{UserID: t.Name()}
			infos, err := c.FetchModels(acct)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 {
				t.Fatalf("unexpected catalog: %+v", infos)
			}
			info, ok := AccountModelInfo(acct.UserID, "catalog-output-model")
			if !ok || info.OutputLimit() != 393216 || info.OutputLimitSource() != "output_context_window" ||
				info.MaxTokens != 65536 || info.OutputContextWindow != 393216 || info.ContextWindow != 1048576 {
				t.Fatalf("desktop budget precedence/raw fields lost: %+v", info)
			}
			wantChannel := ""
			if source == MaasBenefit || source == MaasEducation {
				wantChannel = source
			}
			if ModelChannel(info) != wantChannel {
				t.Errorf("wrong source channel: %+v", info)
			}
		})
	}
}

func TestModelOutputLimitPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		max, output int64
		want        int64
		wantSource  string
	}{
		{"output_above_max", 65536, 393216, 393216, "output_context_window"},
		{"output_below_max", 65536, 8192, 8192, "output_context_window"},
		{"max_fallback", 32768, 0, 32768, "max_tokens"},
		{"invalid_output", 32768, -1, 32768, "max_tokens"},
		{"unknown", 0, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := ModelInfo{MaxTokens: tc.max, OutputContextWindow: tc.output}
			if info.OutputLimit() != tc.want || info.OutputLimitSource() != tc.wantSource {
				t.Fatalf("wrong budget/source: %d/%q", info.OutputLimit(), info.OutputLimitSource())
			}
		})
	}
}

func TestBuiltinCatalogKeepsEntitlementMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpModelBuiltin:
			io.WriteString(w, `{"builtinModels":[
				{"model_id":"catalog-tag-benefit","is_free_benefit":true,"output_context_window":32768},
				{"model_id":"catalog-tag-education","maas_type":"education","max_tokens":8192}
			]}`)
		case EpBenefitGateway, EpEducationGateway:
			io.WriteString(w, `{"enabled":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	acct := &auth.Auth{UserID: t.Name()}
	if _, err := c.FetchModels(acct); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, channel string }{
		{"catalog-tag-benefit", MaasBenefit}, {"catalog-tag-education", MaasEducation},
	} {
		if got := AccountModelChannel(acct.UserID, tc.id); got != tc.channel {
			t.Errorf("builtin catalog tag lost for %s: %q", tc.id, got)
		}
	}
}

func TestAccountDefaultKeepsChannelAndBudgetTogether(t *testing.T) {
	ordinary := ModelInfo{ID: "catalog-shared", Name: "Shared Model", MaxTokens: 65536, OutputContextWindow: 393216}
	benefit := ModelInfo{ID: "CATALOG-SHARED", Name: "Trial Model", Benefit: true, MaxTokens: 8192}
	education := ModelInfo{ID: "Catalog-Shared", Name: "Education Model", MaasType: MaasEducation, OutputContextWindow: 32768}
	for _, tc := range []struct {
		name  string
		infos []ModelInfo
		want  ModelInfo
	}{
		{"ordinary_first", []ModelInfo{ordinary, benefit, education}, ordinary},
		{"ordinary_last", []ModelInfo{education, benefit, ordinary}, ordinary},
		{"benefit_fallback", []ModelInfo{education, benefit}, benefit},
		{"education_only", []ModelInfo{education}, education},
		{"unknown_ordinary_budget", []ModelInfo{benefit, {ID: ordinary.ID}}, ModelInfo{ID: ordinary.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetAccountModels(t.Name(), tc.infos)
			got, ok := AccountModelInfo(t.Name(), "catalog-shared")
			if !ok || got.ID != tc.want.ID || got.OutputLimit() != tc.want.OutputLimit() ||
				ModelChannel(got) != ModelChannel(tc.want) || got.Name != tc.want.Name {
				t.Fatalf("mixed default record: got=%+v want=%+v", got, tc.want)
			}
			if channel := AccountModelChannel(t.Name(), "catalog-shared"); channel != ModelChannel(got) {
				t.Fatalf("route differs from budget record: %q / %+v", channel, got)
			}
			infos, _ := AccountModels(t.Name())
			if len(infos) != len(tc.infos) {
				t.Fatalf("lost channel variants: %+v", infos)
			}
		})
	}
}
