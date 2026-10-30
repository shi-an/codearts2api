package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

func TestClientChannelDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name               string
		benefit, education bool
	}{
		{"closed", false, false},
		{"benefit", true, false},
		{"education", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claims, configs atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				channel := r.URL.Query().Get("channel")
				if channel == "" {
					channel = MaasBenefit
				}
				switch r.URL.Path {
				case EpBenefitGateway, EpEducationGateway, EpBenefitClaim:
					if r.Header.Get("Agent-Type") != "PromptCenter" ||
						!strings.Contains(r.Header.Get("Authorization"), "agent-type") ||
						r.Header.Get("X-Security-Token") != "ST" {
						t.Errorf("client signed headers missing for %s", r.URL.Path)
					}
				}
				switch r.URL.Path {
				case EpModelBuiltin:
					io.WriteString(w, `{"builtinModels":[{"model_id":"GLM-5.2"}]}`)
				case EpBenefitGateway:
					if tc.benefit {
						io.WriteString(w, `{"enabled":true}`)
					} else {
						io.WriteString(w, `{"enabled":false}`)
					}
				case EpEducationGateway:
					if tc.education {
						io.WriteString(w, `{"result":{"enabled":true}}`)
					} else {
						io.WriteString(w, `{"result":{"enabled":false}}`)
					}
				case EpBenefitClaim, EpBenefitConfig:
					if (channel == MaasBenefit && !tc.benefit) || (channel == MaasEducation && !tc.education) {
						t.Errorf("disabled %s channel was requested", channel)
					}
					if r.URL.Path == EpBenefitClaim {
						claims.Add(1)
						body, _ := io.ReadAll(r.Body)
						if r.Method != http.MethodPost || string(body) != "{}" {
							t.Errorf("invalid client claim: method=%s body=%s", r.Method, body)
						}
						io.WriteString(w, `{"error_code":"0000"}`)
					} else {
						configs.Add(1)
						if channel == MaasEducation {
							io.WriteString(w, `{"error_code":"0000","result":{"models":[{"modelId":"edu-client-model","modelName":"Education","description":"edu","context_window":32768}]}}`)
						} else {
							io.WriteString(w, `{"error_code":"0000","result":{"models":[{"model_id":"glm-5.3-flash"}]}}`)
						}
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase, c.benefitBase = srv.URL, srv.URL
			acct := &auth.Auth{UserID: "client-channels-" + tc.name, AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
			infos, err := c.FetchModels(acct)
			if err != nil {
				t.Fatal(err)
			}
			wantChannels := 0
			if tc.benefit {
				wantChannels++
			}
			if tc.education {
				wantChannels++
			}
			if len(infos) != 1+wantChannels || claims.Load() != int32(wantChannels) || configs.Load() != int32(wantChannels) {
				t.Fatalf("models=%+v claims=%d configs=%d", infos, claims.Load(), configs.Load())
			}
			if tc.benefit && AccountModelChannel(acct.UserID, "GLM-5.3-FLASH") != MaasBenefit {
				t.Fatal("benefit route missing")
			}
			if !tc.benefit && (!AccountModelGatewayDisabled(acct.UserID, "glm-5.3-flash") || IsBenefitModel(acct.UserID, "glm-5.3-flash")) {
				t.Fatal("disabled benefit channel must override old model seeds")
			}
			if tc.education && AccountModelChannel(acct.UserID, "EDU-CLIENT-MODEL") != MaasEducation {
				t.Fatal("education route missing")
			}
			c.SetBenefitAutoClaim(false)
			if _, err := c.FetchModels(acct); err != nil {
				t.Fatal(err)
			}
			if claims.Load() != int32(wantChannels) {
				t.Fatal("auto-claim=false still wrote to account")
			}
		})
	}
}

func TestChannelFailureAndClosureUpdateReturnedCatalog(t *testing.T) {
	var phase atomic.Int32
	var benefitConfigs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case EpModelBuiltin:
			io.WriteString(w, `{"builtinModels":[{"model_id":"GLM-5.2"}]}`)
		case EpBenefitGateway:
			if phase.Load() == 2 {
				io.WriteString(w, `{"enabled":false}`)
			} else {
				io.WriteString(w, `{"enabled":true}`)
			}
		case EpEducationGateway:
			io.WriteString(w, `{"enabled":true}`)
		case EpBenefitConfig:
			if r.URL.Query().Get("channel") == MaasEducation {
				io.WriteString(w, `{"error_code":"0000","result":{"models":[{"model_id":"education-stable"}]}}`)
			} else {
				benefitConfigs.Add(1)
				if phase.Load() == 0 {
					io.WriteString(w, `{"error_code":"0000","result":{"models":[{"model_id":"nonseed-trial"}]}}`)
				} else {
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.SetBenefitAutoClaim(false)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	acct := &auth.Auth{UserID: "channel-refresh", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	for n := int32(0); n < 4; n++ {
		phase.Store(n)
		infos, err := c.FetchModels(acct)
		if err != nil {
			t.Fatal(err)
		}
		want := 3
		if n >= 2 {
			want = 2
		}
		if len(infos) != want {
			t.Fatalf("phase=%d returned models=%+v", n, infos)
		}
		if n < 2 && AccountModelChannel(acct.UserID, "nonseed-trial") != MaasBenefit {
			t.Fatal("transient failure lost the confirmed route")
		}
		if n >= 2 && IsBenefitModel(acct.UserID, "nonseed-trial") {
			t.Fatal("closed channel was resurrected by a later source failure")
		}
		if AccountModelChannel(acct.UserID, "education-stable") != MaasEducation {
			t.Fatal("one failing channel affected the successful education channel")
		}
	}
	if benefitConfigs.Load() != 2 {
		t.Fatalf("disabled/backed-off channel requested %d times", benefitConfigs.Load())
	}
}

func TestGatewayFailureIsNotDisabled(t *testing.T) {
	for _, payload := range []string{`{}`, `{"error_code":"4000","enabled":false}`, `{"enabled":"false"}`} {
		t.Run(payload, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, payload)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			_, enabled, err := c.fetchChannelModels("unknown-gateway", SignCredential{}, MaasBenefit)
			if err == nil || enabled != nil {
				t.Fatalf("failed gateway was interpreted as closed: enabled=%v err=%v", enabled, err)
			}
		})
	}
}

func TestChannelConfigRequiresModelsArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpBenefitGateway {
			io.WriteString(w, `{"enabled":true}`)
		} else {
			io.WriteString(w, `{"error_code":"0000","result":{}}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.SetBenefitAutoClaim(false)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	_, enabled, err := c.fetchChannelModels("malformed-models", SignCredential{}, MaasBenefit)
	if err == nil || enabled == nil || !*enabled {
		t.Fatalf("malformed model list must be a source failure, enabled=%v err=%v", enabled, err)
	}
}

func TestEducationRoutePerAccountAndSignedChat(t *testing.T) {
	SetAccountModels("education-user", []ModelInfo{{ID: "Shared-Model", Benefit: true, MaasType: MaasEducation}})
	SetAccountModels("ordinary-user", []ModelInfo{{ID: "Shared-Model"}})
	if AccountModelChannel("education-user", "shared-model") != MaasEducation || AccountModelChannel("ordinary-user", "shared-model") != "" {
		t.Fatal("channel leaked between accounts")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpChatV2 || r.Header.Get(HeaderMaasType) != MaasEducation || !strings.Contains(r.Header.Get("Authorization"), HeaderMaasType) {
			t.Errorf("wrong client route/signature: path=%s channel=%s", r.URL.Path, r.Header.Get(HeaderMaasType))
		}
		io.WriteString(w, "data: {}\n\n")
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}
	rc, err := c.ChatStreamWithChannel(context.Background(), "chat-id", []ChatMessage{{Role: "user", Content: "hi"}}, "trace", cred, "", "Shared-Model", ChatOptions{}, AccountModelChannel("education-user", "shared-model"))
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
}
