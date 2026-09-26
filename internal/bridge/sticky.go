package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cline's gateway ignores provider pinning fields in the request body. The one
// lever it honours is session affinity keyed by X-Task-Id (echoed back as
// routing.clientSessionId): the first request of a task lands on the model's
// plan head (affinity "no_pin"), and later requests with the same task ID are
// pinned by the gateway itself ("confirmed"). The bridge therefore keeps one
// task ID per (credential, upstream model) and reuses it across requests.
const stickyHeader = "X-Task-Id"
const stickyFile = "sticky.json"

const (
	stickyReuse      = "reuse"
	stickyPerRequest = "per_request"
	stickyOff        = "off"
)

type stickySession struct {
	CredentialID   string    `json:"credential_id"`
	Credential     string    `json:"credential"`
	Model          string    `json:"model"`
	TaskID         string    `json:"task_id"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	Requests       int64     `json:"requests"`
	Affinity       string    `json:"affinity"`
	PinnedProvider string    `json:"pinned_provider"`
	PlanHead       string    `json:"plan_head"`
	PlanSize       int       `json:"plan_size"`
	LastProvider   string    `json:"last_provider"`
	Target         string    `json:"target"`
	Rotations      int       `json:"rotations"`
	RotatedFrom    string    `json:"rotated_from,omitempty"`
	Warmups        int       `json:"warmups"`
	// Warning is "target_not_plan_head" when the expected channel can never be
	// pinned because the gateway plan puts another channel first.
	Warning string `json:"warning,omitempty"`
	// warming is closed when the warm-up running for this session finishes.
	warming chan struct{}
}

// A warm-up is the smallest request that makes the gateway route a fresh task ID.
const stickyWarmupBody = `{"messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":32}`

func defaultWarmupDelay(attempt int) time.Duration {
	return time.Duration(min(attempt, 5)) * time.Second
}

// A warm-up answers in about a second; a stalled one must not eat the real request's time.
const defaultWarmupTimeout = 20 * time.Second

func (s *Service) warmupDeadline(limit time.Time) time.Time {
	if d := time.Now().Add(s.warmupTimeout); d.Before(limit) {
		return d
	}
	return limit
}

// stickyProbe is one warm-up or confirmation request as reported to the console.
type stickyProbe struct {
	Stage          string `json:"stage"`
	TaskID         string `json:"task_id"`
	Status         int    `json:"status"`
	Provider       string `json:"provider"`
	Affinity       string `json:"affinity,omitempty"`
	PinnedProvider string `json:"pinned_provider,omitempty"`
	PlanHead       string `json:"plan_head,omitempty"`
	PlanSize       int    `json:"plan_size,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
	Rotated        bool   `json:"rotated"`
}

func (p stickyProbe) attempt() Attempt {
	return Attempt{Status: p.Status, Mode: "sticky-" + p.Stage, Provider: p.Provider, ProviderSource: "provider_metadata.gateway.routing.finalProvider", DurationMS: p.DurationMS, Error: p.Error}
}

func stickyKey(credentialID, upstream string) string { return credentialID + "\x00" + upstream }

func sameProvider(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (s *Service) stickySessionLocked(c Credential, upstream string) *stickySession {
	key := stickyKey(c.ID, upstream)
	session := s.sticky[key]
	if session == nil {
		now := time.Now().UTC()
		session = &stickySession{CredentialID: c.ID, Model: upstream, TaskID: id(), Created: now, Updated: now}
		s.sticky[key] = session
	}
	session.Credential = c.Label
	return session
}

// stickyTaskID returns the task ID to send for this credential and upstream
// model, creating a session on first use. It returns "" when disabled, and a
// throwaway ID in per_request mode, where nothing is reused or warmed.
//
// A task ID nobody has used yet is warmed first: the gateway pins whichever
// channel serves a session's first request, so a tiny request goes out before
// the real one, and a session that lands off target is replaced and retried.
// The real request then streams on a session already served by the target.
func (s *Service) stickyTaskID(r *ExecutorRequest, c Credential, upstream string) string {
	cfg := s.config()
	if cfg.StickyMode == stickyOff || c.ID == "" {
		return ""
	}
	if cfg.StickyMode == stickyPerRequest {
		return id()
	}
	s.stickyMu.Lock()
	session := s.stickySessionLocked(c, upstream)
	if wait := session.warming; wait != nil {
		// Concurrent requests share one warm-up rather than each running their own.
		s.stickyMu.Unlock()
		s.waitWarmup(wait, r.deadline)
		s.stickyMu.Lock()
		defer s.stickyMu.Unlock()
		return s.stickySessionLocked(c, upstream).TaskID
	}
	if session.Requests > 0 || cfg.StickyWarmupAttempts == 0 || r.skipWarmup {
		defer s.stickyMu.Unlock()
		return session.TaskID
	}
	done := make(chan struct{})
	session.warming = done
	s.stickyMu.Unlock()
	for _, probe := range s.warmSticky(r, c, upstream, cfg.StickyWarmupAttempts) {
		r.warmups = append(r.warmups, probe.attempt())
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	session.warming = nil
	close(done)
	return s.stickySessionLocked(c, upstream).TaskID
}

func (s *Service) waitWarmup(done chan struct{}, deadline time.Time) {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-done:
	case <-timeout:
	case <-s.stopCh:
	}
}

// warmSticky runs up to attempts warm-ups, replacing the session each time it
// lands off target. It stops on success, on a client error such as an exhausted
// plan, or when the next pause would use more than half of the request budget.
func (s *Service) warmSticky(r *ExecutorRequest, c Credential, upstream string, attempts int) []stickyProbe {
	deadline := r.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	limit := time.Now().Add(time.Until(deadline) / 2)
	probes := []stickyProbe{}
	for n := 1; n <= attempts; n++ {
		s.stickyMu.Lock()
		task := s.stickySessionLocked(c, upstream).TaskID
		s.stickyMu.Unlock()
		probe, entry, err := s.stickyRequest(r, c, upstream, task, "warmup", s.warmupDeadline(limit))
		probe.Rotated = s.applySticky(entry, true)
		probes = append(probes, probe)
		// A server error or timeout means nothing was served on this task ID; try it again.
		if !probe.Rotated && (err == nil || statusOf(err) < 500) || n == attempts {
			break
		}
		wait := s.warmupDelay(n)
		if time.Now().Add(wait).After(limit) {
			break
		}
		select {
		case <-time.After(wait):
		case <-s.stopCh:
			return probes
		}
	}
	return probes
}

// stickyRequest sends one warm-up or confirmation request on the given task ID.
func (s *Service) stickyRequest(r *ExecutorRequest, c Credential, upstream, task, stage string, deadline time.Time) (stickyProbe, LogEntry, error) {
	j, _ := decodeObject([]byte(stickyWarmupBody))
	j["model"] = upstream
	req := *r
	req.taskID, req.deadline = task, deadline
	entry := LogEntry{Model: r.Model, UpstreamModel: upstream, Provider: "unknown", TaskID: task, stickyKey: stickyKey(c.ID, upstream)}
	attempt := Attempt{Provider: "unknown"}
	start := time.Now()
	up, err := s.request(req, c, j, true)
	if err == nil {
		_, err = s.consumeSSE(up, r.Model, &entry, &attempt, start, nil)
	}
	probe := stickyProbe{Stage: stage, TaskID: task, Status: statusOf(err), Provider: entry.Provider, Affinity: entry.Affinity, PinnedProvider: entry.PinnedProvider, PlanHead: entry.PlanHead, PlanSize: entry.PlanSize, DurationMS: time.Since(start).Milliseconds(), Error: redactModelTest(safeError(err), c)}
	return probe, entry, err
}

// observeSticky applies the gateway's routing facts for one finished request.
// When the session is served by, or pinned to, a channel other than the target
// it is replaced so the next request lands on the plan head again.
func (s *Service) observeSticky(entry LogEntry) bool { return s.applySticky(entry, false) }

// applySticky records one request on its session and reports whether the
// session was replaced.
func (s *Service) applySticky(entry LogEntry, warmup bool) bool {
	if entry.TaskID == "" || entry.stickyKey == "" {
		return false
	}
	expected := ""
	cfg := s.config()
	for _, m := range cfg.Models {
		if m.ID == entry.Model {
			expected = m.StickyProvider
		}
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	session := s.sticky[entry.stickyKey]
	// Requests still carrying a replaced or reset task ID say nothing about the current session.
	if session == nil || session.TaskID != entry.TaskID {
		return false
	}
	if warmup {
		session.Warmups++
	}
	rotated := false
	session.Requests++
	session.Updated = time.Now().UTC()
	if entry.Affinity != "" || entry.PinnedProvider != "" {
		session.Affinity = entry.Affinity
		session.PinnedProvider = entry.PinnedProvider
	}
	if entry.PlanHead != "" {
		session.PlanHead, session.PlanSize = entry.PlanHead, entry.PlanSize
	}
	actual := entry.Provider
	if sameProvider(actual, "unknown") {
		actual = ""
	}
	if actual != "" {
		session.LastProvider = actual
	}
	session.Target = expected
	if session.Target == "" {
		session.Target = session.PlanHead
	}
	session.Warning = ""
	if expected != "" && session.PlanHead != "" && !sameProvider(expected, session.PlanHead) {
		// Rotating cannot help: a fresh session lands on the plan head again.
		session.Warning = "target_not_plan_head"
	} else if session.Target != "" {
		// A confirmed pin outlives one-off gateway fallbacks, so judge it by the pinned
		// channel. Before confirmation the gateway pins whichever channel served.
		served := actual
		if strings.EqualFold(entry.Affinity, "confirmed") && entry.PinnedProvider != "" {
			served = entry.PinnedProvider
		}
		if served != "" && !sameProvider(served, session.Target) {
			now := time.Now().UTC()
			session.TaskID, session.Created, session.Updated = id(), now, now
			session.Requests, session.Affinity, session.PinnedProvider = 0, "", ""
			session.Rotations++
			session.RotatedFrom = served
			rotated = true
		}
	}
	s.persistStickyLocked(cfg.DataDir)
	return rotated
}

// Callers pass dataDir so stickyMu is never held while taking s.mu.
func (s *Service) persistStickyLocked(dataDir string) {
	s.stickyWriteError = safeError(atomicJSON(filepath.Join(dataDir, stickyFile), s.stickySessionsLocked()))
}

func (s *Service) stickySessionsLocked() []stickySession {
	out := make([]stickySession, 0, len(s.sticky))
	for _, session := range s.sticky {
		out = append(out, *session)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].CredentialID < out[j].CredentialID
	})
	return out
}

func (s *Service) loadSticky(dataDir string) {
	b, err := os.ReadFile(filepath.Join(dataDir, stickyFile))
	if err != nil {
		return
	}
	var sessions []stickySession
	if json.Unmarshal(b, &sessions) != nil {
		return
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	for i := range sessions {
		session := sessions[i]
		if session.CredentialID != "" && session.Model != "" && session.TaskID != "" {
			s.sticky[stickyKey(session.CredentialID, session.Model)] = &session
		}
	}
}

// resetSticky drops matching sessions; empty filters match everything.
func (s *Service) resetSticky(credentialID, model string) int {
	dataDir := s.config().DataDir
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	removed := 0
	for key, session := range s.sticky {
		if (credentialID == "" || session.CredentialID == credentialID) && (model == "" || session.Model == model) {
			delete(s.sticky, key)
			removed++
		}
	}
	if removed > 0 {
		s.persistStickyLocked(dataDir)
	}
	return removed
}

func (s *Service) stickyResponse() (any, error) {
	s.stickyMu.Lock()
	sessions := s.stickySessionsLocked()
	writeError := s.stickyWriteError
	s.stickyMu.Unlock()
	s.mu.RLock()
	for i := range sessions {
		if c, ok := s.creds[sessions[i].CredentialID]; ok {
			sessions[i].Credential = c.Label
		}
	}
	s.mu.RUnlock()
	return managementJSON(200, map[string]any{"mode": s.config().StickyMode, "items": sessions, "persistence_error": writeError})
}

// planOrder reads the gateway's execution order, e.g. "Total execution order:
// deepseek(system) → alibaba(system) → …", and returns its head and length.
func planOrder(routing map[string]any) (string, int) {
	plan := str(routing["planningReasoning"])
	var steps []string
	if _, order, ok := strings.Cut(plan, "execution order:"); ok {
		steps = strings.Split(order, "→")
	} else if _, planned, ok := strings.Cut(plan, "planned for:"); ok {
		planned, _, _ = strings.Cut(planned, ". ")
		steps = strings.Split(planned, ",")
	}
	providers := []string{}
	for _, step := range steps {
		name, _, _ := strings.Cut(step, "(")
		if name = strings.Trim(strings.TrimSpace(name), "."); name != "" {
			providers = append(providers, name)
		}
	}
	if len(providers) == 0 {
		return "", 0
	}
	return providers[0], len(providers)
}

func observeRouting(routing map[string]any, entry *LogEntry) {
	if affinity := object(routing["affinity"]); affinity != nil {
		entry.Affinity = str(affinity["outcome"])
		entry.PinnedProvider = str(affinity["pinnedProvider"])
	}
	if head, size := planOrder(routing); head != "" {
		entry.PlanHead, entry.PlanSize = head, size
	}
}

// testSticky resets one session and runs the whole flow live: warm-ups until the
// target serves, then one confirmation request on the resulting task ID.
func (s *Service) testSticky(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	defer s.active.Done()
	var in struct {
		Model        string `json:"model"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Model == "" || in.CredentialID == "" {
		return managementJSON(400, map[string]any{"error": "请选择模型与测试凭据"})
	}
	cfg := s.config()
	if cfg.StickyMode != stickyReuse {
		return managementJSON(400, map[string]any{"error": "粘滞测试仅适用于“复用会话”模式，请先在设置中切换"})
	}
	upstream, err := s.resolveModel(in.Model)
	if err != nil {
		return managementJSON(400, map[string]any{"error": safeError(err)})
	}
	s.mu.RLock()
	c, ok := s.creds[in.CredentialID]
	revoked := s.revoked[in.CredentialID]
	s.mu.RUnlock()
	if !ok || c.Disabled || c.APIKey == "" || revoked {
		return managementJSON(400, map[string]any{"error": "测试凭据不存在或已停用"})
	}
	// CPA's management callback supports only the global proxy, not auth-specific proxies.
	if strings.TrimSpace(c.ProxyURL) != "" {
		return managementJSON(400, map[string]any{"error": "此 CPA 管理接口暂不支持凭据独立代理的测试，请选择使用全局代理的凭据"})
	}
	lock := string(jsonBytes([]string{"sticky", upstream, c.ID}))
	s.mu.Lock()
	if s.modelTests[lock] || len(s.modelTests) >= 3 {
		s.mu.Unlock()
		return managementJSON(429, map[string]any{"error": "已有测试正在进行，请稍后重试（最多同时 3 个）"})
	}
	if s.modelTests == nil {
		s.modelTests = map[string]bool{}
	}
	s.modelTests[lock] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.modelTests, lock); s.mu.Unlock() }()

	s.resetSticky(c.ID, upstream)
	start := time.Now()
	req := ExecutorRequest{AuthID: c.ID, Model: in.Model, HostCallbackID: r.HostCallbackID, deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second)}
	probes := s.warmSticky(&req, c, upstream, max(cfg.StickyWarmupAttempts, 1))
	s.stickyMu.Lock()
	task := s.stickySessionLocked(c, upstream).TaskID
	s.stickyMu.Unlock()
	confirm, observed, _ := s.stickyRequest(&req, c, upstream, task, "confirm", s.warmupDeadline(req.deadline))
	confirm.Rotated = s.applySticky(observed, false)
	probes = append(probes, confirm)

	s.stickyMu.Lock()
	session := *s.stickySessionLocked(c, upstream)
	s.stickyMu.Unlock()
	session.warming = nil
	pinned := confirm.Status == 200 && strings.EqualFold(confirm.Affinity, "confirmed") && session.Target != "" && sameProvider(confirm.PinnedProvider, session.Target)
	entry := LogEntry{ID: id(), Time: time.Now().UTC(), Model: in.Model, UpstreamModel: upstream, Stream: true, Status: confirm.Status, Provider: confirm.Provider, ProviderSource: "provider_metadata.gateway.routing.finalProvider", Credential: c.Label, TaskID: task, Affinity: confirm.Affinity, PinnedProvider: confirm.PinnedProvider, PlanHead: confirm.PlanHead, PlanSize: confirm.PlanSize, DurationMS: time.Since(start).Milliseconds(), Error: confirm.Error}
	for _, probe := range probes {
		entry.Attempts = append(entry.Attempts, probe.attempt())
	}
	s.appendLog(entry)
	return managementJSON(200, map[string]any{"ok": pinned, "target": session.Target, "probes": probes, "session": session, "duration_ms": entry.DurationMS, "tested_at": time.Now().UTC()})
}
