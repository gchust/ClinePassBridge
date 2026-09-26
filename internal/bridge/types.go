package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Version = "0.1.6"
const Provider = "cline-pass"
const PluginID = "clinepassbridge"

type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string           { return e.Message }
func (e *APIError) StatusCode() int         { return e.Status }
func (e *APIError) Code() string            { return e.Kind }
func fail(status int, message string) error { return &APIError{status, "upstream_error", message} }

type HostCall func(string, any, any) error
type ExecutorRequest struct {
	deadline                                               time.Time
	AuthID, AuthProvider, Model, Format, SourceFormat, Alt string
	Stream                                                 bool
	Headers                                                http.Header
	Query                                                  url.Values
	OriginalRequest, Payload, StorageJSON                  []byte
	Metadata, AuthMetadata                                 map[string]any
	AuthAttributes                                         map[string]string
	StreamID                                               string `json:"stream_id"`
	HostCallbackID                                         string `json:"host_callback_id"`
	taskID                                                 string
	warmups                                                []Attempt
	skipWarmup                                             bool
}
type Response struct {
	Payload  []byte
	Headers  http.Header
	Metadata map[string]any `json:",omitempty"`
}
type ManagementRequest struct {
	Method, Path   string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}
type ManagementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type Credential struct {
	Type                string             `json:"type"`
	ID                  string             `json:"id"`
	Label               string             `json:"label"`
	APIKey              string             `json:"api_key"`
	Disabled            bool               `json:"disabled"`
	ProxyURL            string             `json:"proxy_url,omitempty"`
	RequestScopedErrors []RequestErrorRule `json:"request_scoped_errors"`
	ModelRevision       string             `json:"model_revision,omitempty"`
}
type RequestErrorRule struct {
	Status int      `json:"status"`
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

func requestErrorRules() []RequestErrorRule {
	return []RequestErrorRule{{Status: 500, Match: []string{"empty response content"}, Action: "stop"}}
}

type Model struct {
	ID         string   `json:"id" yaml:"id"`
	UpstreamID string   `json:"upstream_id" yaml:"upstream_id"`
	Providers  []string `json:"providers" yaml:"providers"`
	// StickyProvider is the channel the sticky session is expected to hold.
	// Empty means the model's plan head, the only channel the gateway can pin.
	StickyProvider string `json:"sticky_provider,omitempty" yaml:"sticky_provider,omitempty"`
}
type Config struct {
	DataDir          string  `json:"data_dir" yaml:"data_dir"`
	BaseURL          string  `json:"base_url" yaml:"base_url"`
	Models           []Model `json:"models" yaml:"models"`
	NonstreamMode    string  `json:"nonstream_mode" yaml:"nonstream_mode"`
	TimeoutSeconds   int     `json:"timeout_seconds" yaml:"timeout_seconds"`
	LogRetention     int     `json:"log_retention" yaml:"log_retention"`
	MaxResponseBytes int     `json:"max_response_bytes" yaml:"max_response_bytes"`
	// StickyMode is "reuse" (one task ID per credential and model), "per_request"
	// (a new task ID on every request, so nothing is pinned) or "off" (no header;
	// the gateway then falls back to its own per-key default session).
	StickyMode string `json:"sticky_mode" yaml:"sticky_mode"`
	// StickyWarmupAttempts bounds the warm-ups for a fresh session; 0 disables them.
	StickyWarmupAttempts int `json:"sticky_warmup_attempts" yaml:"sticky_warmup_attempts"`
}

const minLogRetention, maxLogRetention = 50, 99999999
const maxStickyWarmupAttempts = 20

func defaultConfig() Config {
	return Config{DataDir: "plugins/clinepassbridge-data", BaseURL: "https://api.cline.bot/api/v1", Models: []Model{}, NonstreamMode: "stream-aggregate", TimeoutSeconds: 180, LogRetention: 1000, MaxResponseBytes: 16 << 20, StickyMode: stickyReuse, StickyWarmupAttempts: 10}
}
func (c *Config) validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "https" || u.Host != "api.cline.bot" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/api/v1" {
		return fail(400, "base_url must be https://api.cline.bot/api/v1")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "timeout_seconds must be between 10 and 1800")
	}
	if c.LogRetention < minLogRetention || c.LogRetention > maxLogRetention {
		return fail(400, fmt.Sprintf("log_retention must be between %d and %d", minLogRetention, maxLogRetention))
	}
	if c.StickyMode == "" {
		c.StickyMode = stickyReuse
	}
	if c.StickyMode != stickyReuse && c.StickyMode != stickyPerRequest && c.StickyMode != stickyOff {
		return fail(400, "sticky_mode must be reuse, per_request or off")
	}
	if c.StickyWarmupAttempts < 0 || c.StickyWarmupAttempts > maxStickyWarmupAttempts {
		return fail(400, fmt.Sprintf("sticky_warmup_attempts must be between 0 and %d", maxStickyWarmupAttempts))
	}
	if c.MaxResponseBytes < 65536 || c.MaxResponseBytes > 64<<20 {
		return fail(400, "max_response_bytes must be between 64 KiB and 64 MiB")
	}
	if c.NonstreamMode != "native" && c.NonstreamMode != "native-fallback" && c.NonstreamMode != "stream-aggregate" {
		return fail(400, "invalid nonstream_mode")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if strings.TrimSpace(m.ID) == "" || seen[m.ID] {
			return fail(400, "model aliases must be nonempty and unique")
		}
		seen[m.ID] = true
		if strings.TrimSpace(m.UpstreamID) == "" || strings.ContainsAny(m.ID+m.UpstreamID, "\r\n\t") {
			return fail(400, "model identifiers must be nonempty and contain no control whitespace")
		}
		m.StickyProvider = strings.TrimSpace(m.StickyProvider)
		if len(m.StickyProvider) > 64 || strings.ContainsAny(m.StickyProvider, "\r\n\t") {
			return fail(400, "sticky_provider must be at most 64 characters without control whitespace")
		}
	}
	return nil
}

type Attempt struct {
	Status         int    `json:"status"`
	Mode           string `json:"mode"`
	Provider       string `json:"provider"`
	ProviderSource string `json:"provider_source"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
}
type LogEntry struct {
	ID               string    `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"`
	Provider         string    `json:"provider"`
	ProviderSource   string    `json:"provider_source"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMS           int64     `json:"ttft_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	Credential       string    `json:"credential"`
	Attempts         []Attempt `json:"attempts"`
	Error            string    `json:"error,omitempty"`
	// Session affinity facts reported by the gateway for the X-Task-Id sent.
	TaskID         string `json:"task_id,omitempty"`
	Affinity       string `json:"affinity,omitempty"`
	PinnedProvider string `json:"pinned_provider,omitempty"`
	PlanHead       string `json:"plan_head,omitempty"`
	PlanSize       int    `json:"plan_size,omitempty"`
	stickyKey      string
}

func jsonBytes(v any) []byte      { b, _ := json.Marshal(v); return b }
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
func decodeObject(b []byte) (map[string]any, error) {
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil || j == nil {
		return nil, fail(502, "upstream returned invalid JSON")
	}
	return j, nil
}
func errorMessage(j map[string]any) string {
	v := j["error"]
	if m := object(v); m != nil {
		return str(m["message"])
	}
	if s := str(v); s != "" {
		return s
	}
	return "upstream request failed"
}
func statusOf(err error) int {
	if err == nil {
		return 200
	}
	if e, ok := err.(interface{ StatusCode() int }); ok && e.StatusCode() > 0 {
		return e.StatusCode()
	}
	return 502
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return fmt.Sprintf("%s", secretPattern.ReplaceAllString(s, "[REDACTED]"))
}
