package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

func TestEnsureAccountModelsDiscoversAndCachesOutputLimit(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpAgentList:
			lists.Add(1)
			io.WriteString(w, `{"agents":[{"agent_id":"a1","is_primary_agent":true}]}`)
		case EpAgentDetail:
			io.WriteString(w, `{"gpts":{"models":[{"model_name":"Limit Test DeepSeek","model_parameters":{"model_id":"deepseek-v4.1-flash-1017","max_tokens":65536,"context_window":1048576}}]}}`)
		case EpModelBuiltin:
			// 内置列表只有 ID 和名称；不能覆盖 agent-center 的完整参数。
			io.WriteString(w, `{"builtinModels":[{"model_id":"deepseek-v4.1-flash-1017"}]}`)
		case EpBenefitGateway, EpEducationGateway:
			io.WriteString(w, `{"enabled":false}`)
		default:
			t.Errorf("unexpected model request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	acct := &auth.Auth{UserID: t.Name()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := c.EnsureAccountModels(ctx, acct); err != nil {
				t.Errorf("ensure model catalog: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if err := c.EnsureAccountModels(ctx, acct); err != nil {
		t.Fatal(err)
	}
	if got := lists.Load(); got != 1 {
		t.Fatalf("concurrent/cached model requests fetched catalog %d times", got)
	}
	info, ok := AccountModelInfo(acct.UserID, "Limit Test DeepSeek")
	if !ok || info.ID != "deepseek-v4.1-flash-1017" || info.MaxTokens != 65536 || info.ContextWindow != 1048576 {
		t.Fatalf("lost account model parameters: %+v, found=%v", info, ok)
	}
	if _, ok := AccountModelInfo(acct.UserID+"-other", info.ID); ok {
		t.Fatal("model limits leaked into another account")
	}
}

func TestEnsureAccountModelsDeadlinePreservesCatalogAndBacksOff(t *testing.T) {
	const model = "model-limit-deadline"
	acct := &auth.Auth{UserID: t.Name()}
	SetAccountModels(acct.UserID, []ModelInfo{{ID: model, MaxTokens: 8192, MaasType: MaasEducation}})
	catalog.Lock()
	catalog.accounts[acct.UserID].fetched = time.Now().Add(-2 * CatalogTTL)
	catalog.Unlock()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case EpAgentList:
			io.WriteString(w, `{"agents":[{"agent_id":"a1","is_primary_agent":true}]}`)
		case EpAgentDetail:
			io.WriteString(w, `{"gpts":{"models":[{"model_parameters":{"model_id":"model-limit-deadline","max_tokens":65536}}]}}`)
		case EpModelBuiltin:
			<-r.Context().Done()
		default:
			t.Errorf("continued model fetch after cancellation: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase, c.benefitBase = srv.URL, srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.EnsureAccountModels(ctx, acct); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("model discovery ignored deadline: %v", err)
	}
	before := requests.Load()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if err := c.EnsureAccountModels(ctx2, acct); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed model discovery did not back off: %v", err)
	}
	if got := requests.Load(); got != before {
		t.Fatalf("retried unavailable model source: requests=%d, before=%d", got, before)
	}
	info, ok := AccountModelInfo(acct.UserID, model)
	if !ok || info.MaxTokens != 8192 || ModelChannel(info) != MaasEducation {
		t.Fatalf("partial discovery replaced previous model parameters: %+v", info)
	}
}
