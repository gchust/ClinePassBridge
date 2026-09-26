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
	// Warning is "target_not_plan_head" when the expected channel can never be
	// pinned because the gateway plan puts another channel first.
	Warning string `json:"warning,omitempty"`
}

func stickyKey(credentialID, upstream string) string { return credentialID + "\x00" + upstream }

func sameProvider(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// stickyTaskID returns the task ID to send for this credential and upstream
// model, creating a session on first use. It returns "" when disabled.
func (s *Service) stickyTaskID(c Credential, upstream string) string {
	if !s.config().StickySession || c.ID == "" {
		return ""
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	key := stickyKey(c.ID, upstream)
	session := s.sticky[key]
	if session == nil {
		now := time.Now().UTC()
		session = &stickySession{CredentialID: c.ID, Model: upstream, TaskID: id(), Created: now, Updated: now}
		s.sticky[key] = session
	}
	session.Credential = c.Label
	return session.TaskID
}

// observeSticky applies the gateway's routing facts for one finished request.
// When the session is served by, or pinned to, a channel other than the target
// it is replaced so the next request lands on the plan head again.
func (s *Service) observeSticky(entry LogEntry) {
	if entry.TaskID == "" || entry.stickyKey == "" {
		return
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
		return
	}
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
		}
	}
	s.persistStickyLocked(cfg.DataDir)
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
	return managementJSON(200, map[string]any{"enabled": s.config().StickySession, "items": sessions, "persistence_error": writeError})
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
