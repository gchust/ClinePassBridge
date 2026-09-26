package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const gatewayPlan = "System credentials planned for: deepseek, alibaba, baseten. Total execution order: deepseek(system) → alibaba(system) → baseten(system)"

func stickyService(t *testing.T, dataDir, extraYAML string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(dataDir)) + extraYAML
	if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	s.warmupDelay = func(int) time.Duration { return 0 }
	return s
}

// deepseekModel disables warm-ups so each request maps to one upstream call;
// warmModel keeps the default of 10 warm-up attempts.
const warmModel = "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
const deepseekModel = "sticky_warmup_attempts: 0\n" + warmModel

func routing(outcome, pinned, final string) map[string]any {
	r := map[string]any{"finalProvider": final, "resolvedProvider": final, "clientSessionId": "echoed", "clientSessionIdSource": "explicit", "planningReasoning": gatewayPlan}
	if outcome != "" {
		affinity := map[string]any{"outcome": outcome}
		if pinned != "" {
			affinity["pinnedProvider"] = pinned
		}
		r["affinity"] = affinity
	}
	return r
}

func routedPlan(r map[string]any) hostPlan {
	var stream []byte
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}})...)
	stream = append(stream, sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"provider_metadata": map[string]any{"gateway": map[string]any{"routing": r}}}, "finish_reason": "stop"}}})...)
	stream = append(stream, []byte("data: [DONE]\r\n\r\n")...)
	return ssePlan(stream)
}

func credentialRequest(credentialID string) json.RawMessage {
	return jsonBytes(ExecutorRequest{
		Model:          "deepseek-flash",
		Payload:        []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		StorageJSON:    jsonBytes(Credential{Type: Provider, ID: credentialID, Label: credentialID, APIKey: "test-key"}),
		HostCallbackID: "callback-1",
	})
}

func runRequests(t *testing.T, s *Service, plans ...hostPlan) *fakeHost {
	t.Helper()
	h := newFakeHost(plans...)
	s.SetHost(h.call)
	for range plans {
		if _, err := s.Handle("executor.execute", executorRequest("")); err != nil {
			t.Fatalf("execute: %v", err)
		}
	}
	return h
}

func onlySession(t *testing.T, s *Service) stickySession {
	t.Helper()
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	sessions := s.stickySessionsLocked()
	if len(sessions) != 1 {
		t.Fatalf("expected one sticky session, got %#v", sessions)
	}
	return sessions[0]
}

func TestStickySessionReusesTaskIDUntilGatewayConfirmsPin(t *testing.T) {
	dir := t.TempDir()
	s := stickyService(t, dir, deepseekModel)
	// A confirmed pin survives a one-off gateway fallback on the third request.
	h := runRequests(t, s, routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")), routedPlan(routing("confirmed", "deepseek", "baseten")))
	task := h.headers[0].Get(stickyHeader)
	if task == "" {
		t.Fatalf("first request carried no %s header: %#v", stickyHeader, h.headers[0])
	}
	for i, header := range h.headers {
		if header.Get(stickyHeader) != task {
			t.Fatalf("request %d changed task ID: %q != %q", i+1, header.Get(stickyHeader), task)
		}
	}
	session := onlySession(t, s)
	if session.TaskID != task || session.Requests != 3 || session.Affinity != "confirmed" || session.PinnedProvider != "deepseek" || session.PlanHead != "deepseek" || session.PlanSize != 3 || session.Target != "deepseek" || session.LastProvider != "baseten" || session.Rotations != 0 || session.Warning != "" {
		t.Fatalf("unexpected sticky session: %#v", session)
	}
	if s.logs[0].TaskID != task || s.logs[0].Affinity != "no_pin" || s.logs[1].Affinity != "confirmed" || s.logs[1].PinnedProvider != "deepseek" || s.logs[2].Provider != "baseten" || s.logs[2].PlanHead != "deepseek" {
		t.Fatalf("logs lost affinity facts: %#v", s.logs)
	}
	// The gateway remembers the pin, so a restart must keep sending the same task ID.
	restarted := stickyService(t, dir, deepseekModel)
	h = runRequests(t, restarted, routedPlan(routing("confirmed", "deepseek", "deepseek")))
	if got := h.headers[0].Get(stickyHeader); got != task {
		t.Fatalf("restart replaced task ID: %q != %q", got, task)
	}
}

func TestStickySessionRotatesWhenServedOrPinnedElsewhere(t *testing.T) {
	s := stickyService(t, t.TempDir(), deepseekModel)
	h := runRequests(t, s,
		routedPlan(routing("no_pin", "", "alibaba")),             // deepseek failed first; confirming would pin alibaba
		routedPlan(routing("no_pin", "", "deepseek")),            // fresh session lands on the plan head
		routedPlan(routing("confirmed", "baseten", "deepseek")),  // pinned elsewhere despite serving deepseek
		routedPlan(routing("confirmed", "deepseek", "deepseek")), // back on the target
	)
	tasks := []string{}
	for _, header := range h.headers {
		tasks = append(tasks, header.Get(stickyHeader))
	}
	if tasks[0] == tasks[1] || tasks[1] != tasks[2] || tasks[2] == tasks[3] {
		t.Fatalf("unexpected task ID sequence: %#v", tasks)
	}
	session := onlySession(t, s)
	if session.TaskID != tasks[3] || session.Rotations != 2 || session.RotatedFrom != "baseten" || session.Requests != 1 || session.Affinity != "confirmed" {
		t.Fatalf("unexpected sticky session after rotations: %#v", session)
	}
}

func TestStickyTargetOutsidePlanHeadWarnsWithoutRotating(t *testing.T) {
	s := stickyService(t, t.TempDir(), deepseekModel+"    sticky_provider: baseten\n")
	h := runRequests(t, s, routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	if h.headers[0].Get(stickyHeader) != h.headers[1].Get(stickyHeader) {
		t.Fatal("an unreachable target must not churn sessions")
	}
	session := onlySession(t, s)
	if session.Warning != "target_not_plan_head" || session.Target != "baseten" || session.Rotations != 0 {
		t.Fatalf("expected an honest unreachable-target warning: %#v", session)
	}
}

func TestStickySessionsAreScopedPerCredentialAndCanBeDisabled(t *testing.T) {
	s := stickyService(t, t.TempDir(), deepseekModel)
	h := newFakeHost(routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("no_pin", "", "deepseek")))
	s.SetHost(h.call)
	for _, credential := range []string{"credential-a", "credential-b"} {
		if _, err := s.Handle("executor.execute", credentialRequest(credential)); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := h.headers[0].Get(stickyHeader), h.headers[1].Get(stickyHeader); a == "" || a == b {
		t.Fatalf("credentials must not share a task ID: %q, %q", a, b)
	}

	disabled := stickyService(t, t.TempDir(), deepseekModel+"sticky_mode: off\n")
	h = runRequests(t, disabled, routedPlan(routing("", "", "deepseek")))
	if got := h.headers[0].Get(stickyHeader); got != "" || disabled.logs[0].TaskID != "" {
		t.Fatalf("disabled sticky session still sent task ID %q", got)
	}
}

func TestStickyResetIgnoresInFlightRequestsOfOldSession(t *testing.T) {
	s := stickyService(t, t.TempDir(), deepseekModel)
	h := runRequests(t, s, routedPlan(routing("no_pin", "", "deepseek")))
	old := s.logs[0]
	old.stickyKey = stickyKey("credential-1", "cline-pass/deepseek-v4.1-flash")
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "DELETE", Path: apiBase + "/sticky"}))
	if err != nil || string(result.(ManagementResponse).Body) != `{"deleted":1}` {
		t.Fatalf("reset sticky sessions: %v %s", err, result.(ManagementResponse).Body)
	}
	h.plans = append(h.plans, routedPlan(routing("no_pin", "", "deepseek")))
	if _, err := s.Handle("executor.execute", executorRequest("")); err != nil {
		t.Fatal(err)
	}
	fresh := h.headers[1].Get(stickyHeader)
	if fresh == "" || fresh == old.TaskID {
		t.Fatalf("reset must start a new task ID, got %q", fresh)
	}
	// A late response for the old task ID must not touch the new session.
	old.Provider = "alibaba"
	s.observeSticky(old)
	if session := onlySession(t, s); session.TaskID != fresh || session.Rotations != 0 || session.Requests != 1 {
		t.Fatalf("stale observation changed session: %#v", session)
	}
	result, _ = s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "GET", Path: apiBase + "/sticky"}))
	var body struct {
		Mode  string          `json:"mode"`
		Items []stickySession `json:"items"`
	}
	if err := json.Unmarshal(result.(ManagementResponse).Body, &body); err != nil || body.Mode != stickyReuse || len(body.Items) != 1 || body.Items[0].TaskID != fresh {
		t.Fatalf("sticky state endpoint: %v %s", err, result.(ManagementResponse).Body)
	}
}

func TestPlanOrderParsesGatewayPlanningReasoning(t *testing.T) {
	cases := []struct {
		plan string
		head string
		size int
	}{
		{gatewayPlan, "deepseek", 3},
		{"System credentials planned for: baseten, fireworks.", "baseten", 2},
		{"Total execution order: zai(system)", "zai", 1},
		{"", "", 0},
	}
	for _, c := range cases {
		if head, size := planOrder(map[string]any{"planningReasoning": c.plan}); head != c.head || size != c.size {
			t.Errorf("planOrder(%q) = %q, %d; want %q, %d", c.plan, head, size, c.head, c.size)
		}
	}
}

func taskIDs(h *fakeHost) []string {
	tasks := []string{}
	for _, header := range h.headers {
		tasks = append(tasks, header.Get(stickyHeader))
	}
	return tasks
}

func errorPlan(status int, message string) hostPlan {
	return hostPlan{status: status, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(map[string]any{"error": map[string]any{"message": message}}), Done: true}}}
}

func execute(t *testing.T, s *Service, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		if _, err := s.Handle("executor.execute", executorRequest("")); err != nil {
			t.Fatalf("execute %d: %v", i+1, err)
		}
	}
}

func TestStickyWarmupConfigBounds(t *testing.T) {
	cfg := defaultConfig()
	if cfg.StickyWarmupAttempts != 10 {
		t.Fatalf("default warm-up attempts = %d, want 10", cfg.StickyWarmupAttempts)
	}
	for n, valid := range map[int]bool{-1: false, 0: true, 20: true, 21: false} {
		cfg.StickyWarmupAttempts = n
		if err := cfg.validate(); (err == nil) != valid {
			t.Fatalf("sticky_warmup_attempts %d: validate() = %v", n, err)
		}
	}
}

func TestStickyWarmupVerifiesFreshSessionBeforeRealRequest(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	h := newFakeHost(routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	s.SetHost(h.call)
	execute(t, s, 2)
	tasks := taskIDs(h)
	if len(tasks) != 3 || tasks[0] == "" || tasks[0] != tasks[1] || tasks[1] != tasks[2] {
		t.Fatalf("one warm-up then two real requests on one task ID expected: %#v", tasks)
	}
	var warm, real map[string]any
	_ = json.Unmarshal(h.bodies[0], &warm)
	_ = json.Unmarshal(h.bodies[1], &real)
	if warm["model"] != "cline-pass/deepseek-v4.1-flash" || number(warm["max_tokens"]) != 32 || !strings.Contains(string(h.bodies[0]), "Reply with OK only.") || !strings.Contains(string(h.bodies[1]), "hello") {
		t.Fatalf("unexpected warm-up or real body: %s / %s", h.bodies[0], h.bodies[1])
	}
	if a := s.logs[0].Attempts; len(a) != 2 || a[0].Mode != "sticky-warmup" || a[0].Provider != "deepseek" || a[1].Mode != "stream-aggregate" {
		t.Fatalf("warm-up should lead the first request's attempts: %#v", a)
	}
	if len(s.logs[1].Attempts) != 1 {
		t.Fatalf("a used session must not warm up again: %#v", s.logs[1].Attempts)
	}
	session := onlySession(t, s)
	if session.Warmups != 1 || session.Requests != 3 || session.Affinity != "confirmed" || session.Rotations != 0 {
		t.Fatalf("unexpected session after warm-up: %#v", session)
	}
}

func TestStickyWarmupReplacesSessionsUntilTargetServes(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	h := newFakeHost(routedPlan(routing("no_pin", "", "alibaba")), routedPlan(routing("no_pin", "", "alibaba")), routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	s.SetHost(h.call)
	execute(t, s, 1)
	tasks := taskIDs(h)
	if len(tasks) != 4 || tasks[0] == tasks[1] || tasks[1] == tasks[2] || tasks[2] != tasks[3] {
		t.Fatalf("each off-target warm-up must switch sessions and the real request use the verified one: %#v", tasks)
	}
	if a := s.logs[0].Attempts; len(a) != 4 || a[0].Provider != "alibaba" || a[2].Provider != "deepseek" || a[3].Mode != "stream-aggregate" {
		t.Fatalf("attempts should list three warm-ups and the real request: %#v", a)
	}
	session := onlySession(t, s)
	if session.Warmups != 3 || session.Rotations != 2 || session.RotatedFrom != "alibaba" || session.PinnedProvider != "deepseek" {
		t.Fatalf("unexpected session: %#v", session)
	}
}

func TestStickyWarmupStopsAtConfiguredAttempts(t *testing.T) {
	s := stickyService(t, t.TempDir(), "sticky_warmup_attempts: 3\n"+warmModel)
	plans := []hostPlan{}
	for i := 0; i < 4; i++ {
		plans = append(plans, routedPlan(routing("no_pin", "", "alibaba")))
	}
	h := newFakeHost(plans...)
	s.SetHost(h.call)
	execute(t, s, 1)
	tasks := taskIDs(h)
	if len(tasks) != 4 || tasks[3] == tasks[2] || tasks[3] == tasks[1] || tasks[3] == tasks[0] {
		t.Fatalf("after 3 warm-ups the real request goes out on a fresh session: %#v", tasks)
	}
	if session := onlySession(t, s); session.Warmups != 3 || session.Rotations != 4 {
		t.Fatalf("unexpected session after exhausted warm-ups: %#v", session)
	}
}

func TestStickyWarmupRetriesServerErrorsAndStopsOnClientErrors(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	h := newFakeHost(errorPlan(502, "all providers failed"), routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	s.SetHost(h.call)
	execute(t, s, 1)
	if tasks := taskIDs(h); len(tasks) != 3 || tasks[0] != tasks[1] || tasks[1] != tasks[2] {
		t.Fatalf("a server error should retry the same unserved task ID: %#v", tasks)
	}
	if session := onlySession(t, s); session.Warmups != 2 || session.Rotations != 0 {
		t.Fatalf("unexpected session after server error: %#v", session)
	}

	limited := stickyService(t, t.TempDir(), warmModel)
	h = newFakeHost(errorPlan(429, "plan usage limit reached"), routedPlan(routing("no_pin", "", "deepseek")))
	limited.SetHost(h.call)
	execute(t, limited, 1)
	if tasks := taskIDs(h); len(tasks) != 2 || tasks[0] != tasks[1] {
		t.Fatalf("a client error must stop warm-ups at once: %#v", tasks)
	}
}

func TestStickyWarmupKeepsHalfTheRequestBudget(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	s.warmupDelay = func(int) time.Duration { return time.Hour }
	h := newFakeHost(routedPlan(routing("no_pin", "", "alibaba")), routedPlan(routing("no_pin", "", "deepseek")))
	s.SetHost(h.call)
	execute(t, s, 1)
	if tasks := taskIDs(h); len(tasks) != 2 || tasks[0] == tasks[1] {
		t.Fatalf("a pause beyond the budget should end warm-ups on a fresh session: %#v", tasks)
	}
}

func TestConcurrentRequestsShareOneWarmup(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	h := newFakeHost(routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	h.gate, h.gateEntered = make(chan struct{}), make(chan struct{})
	s.SetHost(h.call)
	errs := make(chan error, 2)
	go func() { _, err := s.Handle("executor.execute", executorRequest("")); errs <- err }()
	<-h.gateEntered
	go func() { _, err := s.Handle("executor.execute", executorRequest("")); errs <- err }()
	time.Sleep(50 * time.Millisecond)
	close(h.gate)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	tasks := taskIDs(h)
	if len(tasks) != 3 || tasks[0] != tasks[1] || tasks[1] != tasks[2] {
		t.Fatalf("concurrent requests should share one warm-up: %#v", tasks)
	}
	if session := onlySession(t, s); session.Warmups != 1 {
		t.Fatalf("expected a single warm-up: %#v", session)
	}
}

func TestModelProbeSkipsWarmup(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	s.creds["c1"] = Credential{Type: Provider, ID: "c1", Label: "main", APIKey: "test-key"}
	h := newFakeHost(routedPlan(routing("no_pin", "", "deepseek")))
	s.SetHost(h.call)
	_, err := s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "POST", Path: apiBase + "/models/test", Body: jsonBytes(map[string]any{"model": "deepseek-flash", "upstream_id": "cline-pass/deepseek-v4.1-flash", "credential_id": "c1"})}))
	if err != nil || len(h.opened) != 1 {
		t.Fatalf("model probe should send exactly one request: %v, %d", err, len(h.opened))
	}
}

func stickyTest(t *testing.T, s *Service) (int, map[string]any) {
	t.Helper()
	result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{Method: "POST", Path: apiBase + "/sticky/test", Body: jsonBytes(map[string]any{"model": "deepseek-flash", "credential_id": "c1"})}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(result.(ManagementResponse).Body, &body)
	return result.(ManagementResponse).StatusCode, body
}

func TestStickyTestEndpointRunsWarmupAndConfirmation(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	s.creds["c1"] = Credential{Type: Provider, ID: "c1", Label: "main", APIKey: "test-key"}
	old := &stickySession{CredentialID: "c1", Model: "cline-pass/deepseek-v4.1-flash", TaskID: "old-session", Requests: 9}
	s.sticky[stickyKey("c1", "cline-pass/deepseek-v4.1-flash")] = old
	h := newFakeHost(routedPlan(routing("no_pin", "", "alibaba")), routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	s.SetHost(h.call)
	status, body := stickyTest(t, s)
	probes := list(body["probes"])
	if status != 200 || body["ok"] != true || body["target"] != "deepseek" || len(probes) != 3 {
		t.Fatalf("unexpected sticky test result: %d %#v", status, body)
	}
	first, last := object(probes[0]), object(probes[2])
	if first["stage"] != "warmup" || first["provider"] != "alibaba" || first["rotated"] != true || last["stage"] != "confirm" || last["affinity"] != "confirmed" || last["pinned_provider"] != "deepseek" {
		t.Fatalf("probes should show the switch and the confirmed pin: %#v", probes)
	}
	if tasks := taskIDs(h); tasks[0] == "old-session" || tasks[1] != tasks[2] {
		t.Fatalf("the test must start from a reset session and confirm the verified one: %#v", tasks)
	}
	if a := s.logs[len(s.logs)-1].Attempts; len(a) != 3 || a[0].Mode != "sticky-warmup" || a[2].Mode != "sticky-confirm" {
		t.Fatalf("the test should be logged with all probes: %#v", a)
	}

	unreachable := stickyService(t, t.TempDir(), warmModel+"    sticky_provider: baseten\n")
	unreachable.creds["c1"] = Credential{Type: Provider, ID: "c1", Label: "main", APIKey: "test-key"}
	h = newFakeHost(routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	unreachable.SetHost(h.call)
	if status, body = stickyTest(t, unreachable); status != 200 || body["ok"] != false || len(h.opened) != 2 {
		t.Fatalf("an unreachable target must be reported without churning: %d %#v", status, body)
	}

	disabled := stickyService(t, t.TempDir(), warmModel+"sticky_mode: per_request\n")
	disabled.creds["c1"] = Credential{Type: Provider, ID: "c1", Label: "main", APIKey: "test-key"}
	if status, _ = stickyTest(t, disabled); status != 400 {
		t.Fatalf("sticky test outside reuse mode should be rejected, got %d", status)
	}
}

func TestStreamingRequestStreamsOnWarmedSession(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	h := newFakeHost(routedPlan(routing("no_pin", "", "alibaba")), routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	s.SetHost(h.call)
	if _, err := s.Handle("executor.execute_stream", executorRequest("client-stream")); err != nil {
		t.Fatal(err)
	}
	<-h.clientClosed
	s.active.Wait()
	tasks := taskIDs(h)
	if len(tasks) != 3 || tasks[0] == tasks[1] || tasks[1] != tasks[2] || len(h.emitted) == 0 || h.clientError != "" {
		t.Fatalf("stream should go out on the verified session and reach the client: %#v emitted=%d err=%q", tasks, len(h.emitted), h.clientError)
	}
	if a := s.logs[0].Attempts; len(a) != 3 || a[2].Mode != "stream" || s.logs[0].Affinity != "confirmed" {
		t.Fatalf("stream log should carry warm-ups and the confirmed pin: %#v", s.logs[0])
	}
}

func TestStalledWarmupTimesOutAndRetriesSameSession(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel)
	s.warmupTimeout = 50 * time.Millisecond
	h := newFakeHost(routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("confirmed", "deepseek", "deepseek")))
	h.gate, h.gateEntered = make(chan struct{}), make(chan struct{})
	s.SetHost(h.call)
	done := make(chan error, 1)
	go func() { _, err := s.Handle("executor.execute", executorRequest("")); done <- err }()
	<-h.gateEntered
	// The stalled first warm-up times out on its own budget while the rest proceed.
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(h.gate)
	s.active.Wait()
	a := s.logs[0].Attempts
	if len(a) != 3 || a[0].Status != 504 || a[1].Status != 200 || a[1].Provider != "deepseek" || a[2].Mode != "stream-aggregate" {
		t.Fatalf("a stalled warm-up should time out on its own budget and retry: %#v", a)
	}
	if tasks := taskIDs(h); len(tasks) != 3 || tasks[0] != tasks[1] || tasks[1] != tasks[2] {
		t.Fatalf("a timed-out warm-up must retry the same unserved task ID: %#v", tasks)
	}
}

func TestPerRequestModeSendsFreshTaskIDsWithoutWarmups(t *testing.T) {
	s := stickyService(t, t.TempDir(), warmModel+"sticky_mode: per_request\n")
	h := runRequests(t, s, routedPlan(routing("no_pin", "", "deepseek")), routedPlan(routing("no_pin", "", "deepseek")))
	tasks := taskIDs(h)
	if len(tasks) != 2 || tasks[0] == "" || tasks[1] == "" || tasks[0] == tasks[1] {
		t.Fatalf("per_request mode must send a new task ID on every request and skip warm-ups: %#v", tasks)
	}
	if s.logs[1].TaskID != tasks[1] || s.logs[1].Affinity != "no_pin" || len(s.logs[1].Attempts) != 1 {
		t.Fatalf("logs should still record the task ID and affinity: %#v", s.logs[1])
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	if len(s.sticky) != 0 {
		t.Fatalf("per_request mode must not store sessions: %#v", s.sticky)
	}
}

func TestStickyModeValidation(t *testing.T) {
	cfg := defaultConfig()
	if cfg.StickyMode != stickyReuse {
		t.Fatalf("default sticky mode = %q", cfg.StickyMode)
	}
	for mode, valid := range map[string]bool{"": true, stickyReuse: true, stickyPerRequest: true, stickyOff: true, "always": false} {
		cfg.StickyMode = mode
		if err := cfg.validate(); (err == nil) != valid {
			t.Fatalf("sticky_mode %q: validate() = %v", mode, err)
		}
	}
}
