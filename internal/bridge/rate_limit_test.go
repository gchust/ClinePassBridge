package bridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const teamLimitMessage = `Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'deepseek/deepseek-v4.1-flash' with streaming: request failed with status 429: {"error":{"message":"Rate limit exceeded for deepseek/deepseek-v4.1-flash: this team's limit of 100000000 input tokens per minute (per region) was reached. Retry after 8s."}}`

func limitFrame() []byte {
	return sseFrame(map[string]any{"error": map[string]any{"message": teamLimitMessage}})
}

func TestUpstreamErrorClassification(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, message, code, kind string
		httpStatus, wantStatus    int
		header                    string
		wait                      time.Duration
	}{
		{"wrapped team", teamLimitMessage, "", "upstream_team_rate_limited", 200, 429, "", 8 * time.Second},
		{"native rate", "try later", "rate_limit_exceeded", "upstream_rate_limited", 429, 429, "17", 17 * time.Second},
		{"structured SSE", "try later", "rate_limit_exceeded", "upstream_rate_limited", 200, 429, "", time.Minute},
		{"date header", "busy", "", "upstream_rate_limited", 429, 429, now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"bad header", "busy", "", "upstream_rate_limited", 429, 429, "NaN", time.Minute},
		{"quota", "quota exhausted", "insufficient_quota", "upstream_quota_exhausted", 429, 429, "", 0},
		{"authentication wins", teamLimitMessage, "", "upstream_auth_error", 401, 401, "", 0},
		{"unrelated digits", "request 429 failed but server is unavailable", "", "upstream_error", 200, 502, "", 0},
		{"server error", "service unavailable", "", "upstream_error", 503, 503, "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyUpstreamError(tt.httpStatus, http.Header{"Retry-After": []string{tt.header}}, map[string]any{"error": map[string]any{"message": tt.message, "code": tt.code}}, now)
			detail := err.(*upstreamError)
			if detail.Status != tt.wantStatus || detail.Kind != tt.kind {
				t.Fatalf("classification = %#v", detail)
			}
			if tt.wait > 0 && detail.RetryAt.Sub(now) != tt.wait {
				t.Fatalf("retry=%v error=%v", detail.RetryAt.Sub(now), err)
			}
			if strings.HasPrefix(err.Error(), rateLimitMarker) != (tt.kind == "upstream_team_rate_limited") {
				t.Fatalf("ordinary errors must not match team-only CPA exemption: %v", err)
			}
			if tt.wait == 0 && (asTeamRateLimit(err) != nil || !detail.RetryAt.IsZero()) {
				t.Fatalf("incorrect exemption: %v", err)
			}
		})
	}
}

func TestLimiterSingleProbeAndStaleCompletion(t *testing.T) {
	now := time.Now()
	l := rateLimiter{clock: func() time.Time { return now }}
	c := Credential{ID: "one", APIKey: "fixture"}
	a, _ := l.acquire(c, "model")
	old, _ := l.acquire(c, "model")
	limited := classifyUpstreamError(200, nil, map[string]any{"error": teamLimitMessage}, now)
	a.finish(limited)
	old.finish(nil)
	if _, err := l.acquire(c, "model"); asTeamRateLimit(err) == nil {
		t.Fatal("old success erased current limit")
	}
	if lease, err := l.acquire(Credential{ID: "two", APIKey: "other"}, "model"); err != nil || lease == nil {
		t.Fatal("unrelated credential blocked")
	}
	now = now.Add(9 * time.Second)
	var count atomic.Int32
	var wg sync.WaitGroup
	var probe *rateLease
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := l.acquire(c, "model")
			if err == nil {
				count.Add(1)
				probe = lease
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("recovery probes=%d", count.Load())
	}
	// Another already-running call discovers a newer limit while the probe runs.
	old.finish(classifyUpstreamError(429, http.Header{"Retry-After": []string{"30"}}, map[string]any{"error": teamLimitMessage}, now))
	probe.finish(nil)
	if _, err := l.acquire(c, "model"); asTeamRateLimit(err) == nil {
		t.Fatal("probe success erased newer limit")
	}
	now = now.Add(31 * time.Second)
	probe, err := l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	probe.finish(nil)
	if _, err = l.acquire(c, "model"); err != nil {
		t.Fatal("did not recover", err)
	}
	old.finish(classifyUpstreamError(429, http.Header{"Retry-After": []string{"1"}}, map[string]any{"error": teamLimitMessage}, now))
	now = now.Add(2 * time.Second)
	probe, err = l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	probe.finish(fail(502, "context canceled"))
	if _, err = l.acquire(c, "model"); asTeamRateLimit(err) == nil {
		t.Fatal("uncertain probe reopened traffic")
	}
	now = now.Add(6 * time.Second)
	probe, err = l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.acquire(c, "model"); asTeamRateLimit(err) == nil {
		t.Fatal("uncertain recovery lost single probe")
	}
	probe.finish(nil)
}

func TestStreamRateLimitBeforeOutputAndAliasSuppression(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	s.cfg.Models = append(s.cfg.Models, Model{ID: "other-alias", UpstreamID: s.cfg.Models[0].UpstreamID})
	role := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	h := newFakeHost(ssePlan(role, limitFrame()))
	s.SetHost(h.call)
	_, err := s.Handle("executor.execute_stream", executorRequest("client-1"))
	if asTeamRateLimit(err) == nil || statusOf(err) != 429 {
		t.Fatalf("wanted synchronous structured 429, got %v", err)
	}
	if len(h.emitted) != 0 {
		t.Fatal("prelude escaped before error classification")
	}
	firstUnknown := s.estimates.Keys[estimateKeyID(Credential{APIKey: "test-key"})].Totals.Unknown
	var req ExecutorRequest
	_ = json.Unmarshal(executorRequest("client-2"), &req)
	req.Model = "other-alias"
	_, err = s.Handle("executor.execute_stream", jsonBytes(req))
	if asTeamRateLimit(err) == nil || !asTeamRateLimit(err).Local || len(h.opened) != 1 {
		t.Fatalf("alias bypassed local window: %v opens=%d", err, len(h.opened))
	}
	if len(s.logs) != 2 || s.logs[0].UpstreamHTTPStatus != 200 || s.logs[0].UpstreamErrorStatus != 429 || !s.logs[1].UpstreamSkipped {
		t.Fatalf("classification log=%#v", s.logs)
	}
	if got := s.estimates.Keys[estimateKeyID(Credential{APIKey: "test-key"})].Totals.Unknown; got != firstUnknown {
		t.Fatalf("local block inflated unknown usage: %d => %d", firstUnknown, got)
	}
}

func TestOrdinary429DoesNotUseTeamExemptionOrLocalWindow(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost(ssePlan(sseFrame(map[string]any{"error": map[string]any{"code": "rate_limit_exceeded", "message": "Per-account request rate exceeded"}})), ssePlan(simpleSSE()))
	s.SetHost(h.call)
	_, err := s.Handle("executor.execute_stream", executorRequest("client-1"))
	if statusOf(err) != 429 || asTeamRateLimit(err) != nil || strings.Contains(err.Error(), rateLimitMarker) {
		t.Fatalf("ordinary 429 received team exemption: %v", err)
	}
	if s.logs[0].ErrorKind != "upstream_rate_limited" || s.logs[0].RateLimitScope != "credential_model" {
		t.Fatalf("ordinary classification=%#v", s.logs[0])
	}
	// CPA retains its ordinary-429 policy; the plugin itself must not apply
	// the special shared-team suppression when directly called again.
	if _, err = s.Handle("executor.execute", executorRequest("")); err != nil || len(h.opened) != 2 {
		t.Fatalf("ordinary 429 incorrectly enabled team window: %v opens=%d", err, len(h.opened))
	}
}

func TestPreflightPreservesPreludeAndDoesNotReplayPartialOutput(t *testing.T) {
	role := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}})
	content := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost(ssePlan(role, content, limitFrame()))
	s.SetHost(h.call)
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not terminate")
	}
	if len(h.opened) != 1 || len(h.emitted) != 2 || !strings.Contains(h.clientError, rateLimitMarker) {
		t.Fatalf("opens=%d frames=%d err=%s", len(h.opened), len(h.emitted), h.clientError)
	}
	var frame map[string]any
	_ = json.Unmarshal(h.emitted[0], &frame)
	if str(object(object(list(frame["choices"])[0])["delta"])["role"]) != "assistant" {
		t.Fatal("prelude order lost")
	}
}

func TestShutdownDuringPreflight(t *testing.T) {
	s := registeredService(t, "stream-aggregate")
	reading := make(chan struct{})
	release := make(chan struct{})
	var closeOnce sync.Once
	s.SetHost(func(method string, in, out any) error {
		switch method {
		case "host.http.do_stream":
			*out.(*upstreamStream) = upstreamStream{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}, StreamID: "blocked"}
		case "host.http.stream_read":
			close(reading)
			<-release
			*out.(*readChunk) = readChunk{Error: "context canceled", Done: true}
		case "host.http.stream_close":
			closeOnce.Do(func() { close(release) })
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, err := s.Handle("executor.execute_stream", executorRequest("client-1")); done <- err }()
	select {
	case <-reading:
	case <-time.After(3 * time.Second):
		t.Fatal("preflight not reading")
	}
	if _, err := s.Handle("plugin.shutdown", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("preflight leaked")
	}
}

func TestExistingCredentialRulesAreRefreshedInMetadataAndStorage(t *testing.T) {
	c := Credential{Type: Provider, ID: "one", APIKey: "fixture", Disabled: true, ProxyURL: "http://localhost:1234", RequestScopedErrors: []RequestErrorRule{{Status: 500, Match: []string{"empty response content"}, Action: "stop"}}}
	auth := authData(c, "one.json").(map[string]any)
	var saved Credential
	_ = json.Unmarshal(auth["StorageJSON"].([]byte), &saved)
	if saved.APIKey != c.APIKey || saved.ProxyURL != c.ProxyURL || !saved.Disabled || len(saved.RequestScopedErrors) != 2 {
		t.Fatalf("credential fields/rules changed: %v", saved.RequestScopedErrors)
	}
	meta := auth["Metadata"].(map[string]any)
	rules := meta["request_scoped_errors"].([]any)
	if len(rules) != 2 || object(rules[1])["status"] != float64(429) || object(rules[1])["action"] != "stop" {
		t.Fatalf("CPA metadata rules=%#v", rules)
	}
}
