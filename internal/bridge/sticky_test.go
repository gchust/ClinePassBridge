package bridge

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

const gatewayPlan = "System credentials planned for: deepseek, alibaba, baseten. Total execution order: deepseek(system) → alibaba(system) → baseten(system)"

func stickyService(t *testing.T, dataDir, extraYAML string) *Service {
	t.Helper()
	s := NewService()
	configYAML := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(dataDir)) + extraYAML
	if _, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)})); err != nil {
		t.Fatalf("register plugin: %v", err)
	}
	return s
}

const deepseekModel = "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"

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

	disabled := stickyService(t, t.TempDir(), deepseekModel+"sticky_session: false\n")
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
		Enabled bool            `json:"enabled"`
		Items   []stickySession `json:"items"`
	}
	if err := json.Unmarshal(result.(ManagementResponse).Body, &body); err != nil || !body.Enabled || len(body.Items) != 1 || body.Items[0].TaskID != fresh {
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
