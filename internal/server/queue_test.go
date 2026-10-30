package server

import (
	"context"
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

func queueTestHandler(t *testing.T, endpoints ...string) (*Handler, *pool.Pool) {
	t.Helper()
	var auths []*auth.Auth
	for i := range endpoints {
		uid := t.Name() + string(rune('a'+i))
		auths = append(auths, auth.New(uid, "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range p.Accounts() {
		a.Client = upstream.NewWithChatEndpoint(time.Second, endpoints[i])
	}
	return NewHandler(Config{Pool: p, MaxRotate: len(endpoints), QueueRetryDelay: 200 * time.Millisecond, QueueMaxAttempts: 2}), p
}

func queueTestRequest(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
}

func TestQueueExhaustionReturns429WithoutFinalWait(t *testing.T) {
	var attempts atomic.Int32
	lastAttempt := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 2 {
			close(lastAttempt)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "TPM limit reached")
	}))
	defer srv.Close()
	h, p := queueTestHandler(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, queueTestRequest(ctx)); close(done) }()
	select {
	case <-lastAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("retry was not sent")
	}
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("waited after the final attempt")
	}
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || attempts.Load() != 2 {
		t.Fatalf("status=%d retry-after=%q attempts=%d body=%s", rec.Code, rec.Header().Get("Retry-After"), attempts.Load(), rec.Body.String())
	}
	state := p.List()[0]
	if state["err_count"] != 0 || state["disabled"] != false || state["active_concurrent"] != 0 {
		t.Fatalf("queue limit damaged pool state: %#v", state)
	}
}

func TestQueueWaitKeepsSlotAndCancellationReleasesIt(t *testing.T) {
	seen := make(chan struct{}, 1)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "TPM limit reached")
		seen <- struct{}{}
	}))
	defer srv.Close()
	h, p := queueTestHandler(t, srv.URL)
	h.cfg.QueueRetryDelay = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), queueTestRequest(ctx)); close(done) }()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("no upstream attempt")
	}
	// Every sample during backoff must still see this request's slot reserved.
	for i := 0; i < 20; i++ {
		if !p.Busy(p.Accounts()[0].Name) {
			t.Error("backoff released its slot and allowed other requests to hit the same limit")
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled retry kept waiting")
	}
	if p.Busy(p.Accounts()[0].Name) || !p.Healthy(p.Accounts()[0].Name) || attempts.Load() != 1 {
		t.Fatalf("cancelled retry leaked a slot, cooled the account, or retried: attempts=%d state=%v", attempts.Load(), p.List())
	}
}

func TestQueueLimitTriesReadyAccountBeforeWaiting(t *testing.T) {
	var firstAttempts, secondAttempts atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstAttempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "TPM limit reached")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondAttempts.Add(1)
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "deepseek-v4.1-flash-1017" {
			t.Errorf("failover changed the mapped target: model=%q err=%v", body.Model, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ready\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer second.Close()
	h, _ := queueTestHandler(t, first.URL, second.URL)
	// 已有会话黏性也不能导致下次轮转再次挑回同一限流账号。
	req := queueTestRequest(context.Background())
	parsed, err := parseChatRequest([]byte(`{"model":"deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h.convAcct[conversationIDFor(parsed, "")] = h.cfg.Pool.Accounts()[0].Name
	h.cfg.QueueRetryDelay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK || firstAttempts.Load() != 1 || secondAttempts.Load() != 1 || !strings.Contains(rec.Body.String(), `"content":"ready"`) {
		t.Fatalf("ready account was ignored: status=%d attempts=%d/%d body=%s", rec.Code, firstAttempts.Load(), secondAttempts.Load(), rec.Body.String())
	}
}

func TestUpstreamRetryAfterPreventsEarlyRetryAndPreserves429(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				if status == http.StatusOK {
					io.WriteString(w, "data: {\"error_code\":\"InferHub.ModelArts.81111.429\",\"error_msg\":\"TPM limit reached\"}\n\n")
				} else {
					io.WriteString(w, "TPM limit reached")
				}
			}))
			defer srv.Close()
			h, p := queueTestHandler(t, srv.URL)
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, queueTestRequest(context.Background()))
				if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" || attempts.Load() != 1 {
					t.Fatalf("rate limit retried early or became 503: status=%d retry-after=%q attempts=%d", rec.Code, rec.Header().Get("Retry-After"), attempts.Load())
				}
			}
			if delay := p.QueueRetryAfter(); delay < 59*time.Second || delay > time.Minute {
				t.Fatalf("upstream delay ignored: %s", delay)
			}
		})
	}
}

func TestLateQueueErrorUsesShortBackoffAndReleasesSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"error_code\":\"TM.00001041\",\"error_msg\":\"并发会话数已达上限\"}\n\n")
	}))
	defer srv.Close()
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[stream], func(t *testing.T) {
			h, p := queueTestHandler(t, srv.URL)
			h.cfg.QueueRetryDelay = time.Second
			payload, _ := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash-1017", "stream": stream, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload))))
			want := http.StatusTooManyRequests
			if stream {
				want = http.StatusOK // Stream headers were already sent before the error.
				if !strings.Contains(rec.Body.String(), "event: error") {
					t.Fatalf("missing late stream error: %s", rec.Body.String())
				}
			}
			if rec.Code != want {
				t.Fatalf("late queue error status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			state := p.List()[0]
			if state["active_concurrent"] != 0 || state["err_count"] != 0 {
				t.Fatalf("late queue error leaked slot or counted account failure: %#v", state)
			}
			if delay := p.QueueRetryAfter(); delay <= 0 || delay > time.Second {
				t.Fatalf("queue error got long account-failure cooldown: %s", delay)
			}
		})
	}
}
