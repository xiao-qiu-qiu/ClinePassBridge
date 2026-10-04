package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const Version = "0.2.11"
const Provider = "cline-pass"
const PluginID = "clinepassbridge"

// How a client-supplied provider preference interacts with the per-model one.
const (
	// ProviderPolicyClient lets the caller's own gateway options win. Default.
	ProviderPolicyClient = "client"
	// ProviderPolicyConfig pins every request to the configured providers and
	// rejects caller overrides.
	ProviderPolicyConfig = "config"
)

// A pinned set is a routing allow-list, not a budget. Keep it small enough to
// stay reviewable in the UI.
const maxModelProviders = 8

// localBackoffLabel is the label the UI shows, and filters by, for a request the
// plugin suppressed locally. Such a request never reached an upstream, so it has
// no provider of its own.
const localBackoffLabel = "本地退避"

// maxCredentialPriority bounds the value handed to CPA's scheduler.
const maxCredentialPriority = 100

// normalizedPriority treats zero, negative and absent alike as "no preference",
// so the plugin only ever forwards a real ordering choice.
func normalizedPriority(value *int) *int {
	if value == nil || *value <= 0 {
		return nil
	}
	n := *value
	if n > maxCredentialPriority {
		n = maxCredentialPriority
	}
	return &n
}

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
	Type     string `json:"type"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled"`
	ProxyURL string `json:"proxy_url,omitempty"`
	// Priority is forwarded to CPA, which schedules higher values first.
	// Absent (nil) means "no preference"; the plugin does not invent one.
	Priority            *int               `json:"priority,omitempty"`
	RequestScopedErrors []RequestErrorRule `json:"request_scoped_errors"`
	ModelRevision       string             `json:"model_revision,omitempty"`
	RoutingID           string             `json:"routing_id,omitempty"`
}
type RequestErrorRule struct {
	Status int      `json:"status"`
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

func requestErrorRules() []RequestErrorRule {
	return []RequestErrorRule{
		{Status: 500, Match: []string{"empty response content"}, Action: "stop"},
		{Status: 429, Match: []string{rateLimitMarker}, Action: "stop"},
	}
}

type Model struct {
	ID         string   `json:"id" yaml:"id"`
	UpstreamID string   `json:"upstream_id" yaml:"upstream_id"`
	Providers  []string `json:"providers" yaml:"providers"`
}
type Config struct {
	DataDir          string  `json:"data_dir" yaml:"data_dir"`
	BaseURL          string  `json:"base_url" yaml:"base_url"`
	Models           []Model `json:"models" yaml:"models"`
	NonstreamMode    string  `json:"nonstream_mode" yaml:"nonstream_mode"`
	ProviderPolicy   string  `json:"provider_policy" yaml:"provider_policy"`
	TimeoutSeconds   int     `json:"timeout_seconds" yaml:"timeout_seconds"`
	LogRetention     int     `json:"log_retention" yaml:"log_retention"`
	MaxResponseBytes int     `json:"max_response_bytes" yaml:"max_response_bytes"`
}

func defaultConfig() Config {
	return Config{DataDir: "plugins/clinepassbridge-data", BaseURL: "https://api.cline.bot/api/v1", Models: []Model{}, NonstreamMode: "stream-aggregate", ProviderPolicy: ProviderPolicyConfig, TimeoutSeconds: 600, LogRetention: 1000, MaxResponseBytes: 16 << 20}
}
func (c *Config) validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Scheme != "https" || u.Host != "api.cline.bot" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/api/v1" {
		return fail(400, "base_url must be https://api.cline.bot/api/v1")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "timeout_seconds must be between 10 and 1800")
	}
	if c.LogRetention < 50 || c.LogRetention > 10000 {
		return fail(400, "log_retention must be between 50 and 10000")
	}
	if c.MaxResponseBytes < 65536 || c.MaxResponseBytes > 64<<20 {
		return fail(400, "max_response_bytes must be between 64 KiB and 64 MiB")
	}
	if c.NonstreamMode != "native" && c.NonstreamMode != "native-fallback" && c.NonstreamMode != "stream-aggregate" {
		return fail(400, "invalid nonstream_mode")
	}
	// Settings written before this field existed carry no value. Default to the
	// configured list deciding, so an unconfigured install cannot have its
	// routing changed by whatever a caller happens to send.
	if c.ProviderPolicy == "" {
		c.ProviderPolicy = ProviderPolicyConfig
	}
	if c.ProviderPolicy != ProviderPolicyClient && c.ProviderPolicy != ProviderPolicyConfig {
		return fail(400, "provider_policy must be client or config")
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
		// A fresh install pins DeepSeek models to the vendor's own endpoint.
		// An explicit empty list means the operator cleared it, so only a
		// never-set field is defaulted.
		if m.Providers == nil {
			m.Providers = defaultProvidersFor(m.ID, m.UpstreamID)
		}
		if len(m.Providers) > maxModelProviders {
			return fail(400, "a model can pin at most 8 upstream providers")
		}
		pinned := map[string]bool{}
		for n, raw := range m.Providers {
			slug := strings.ToLower(strings.TrimSpace(raw))
			if slug == "" || strings.ContainsAny(slug, " \r\n\t") {
				return fail(400, "upstream providers must be nonempty slugs without whitespace")
			}
			if pinned[slug] {
				return fail(400, "upstream providers must be unique within a model")
			}
			pinned[slug] = true
			m.Providers[n] = slug
		}
	}
	return nil
}

// defaultProvidersFor supplies the initial allow-list for a model that has never
// been pinned. Only DeepSeek is seeded; other models keep automatic routing.
func defaultProvidersFor(id, upstreamID string) []string {
	if strings.Contains(strings.ToLower(id+" "+upstreamID), "deepseek") {
		return []string{"deepseek"}
	}
	return nil
}

// pinnedProviders returns the configured allow-list for a client-facing model
// alias, preserving the configured order as the priority order.
func (c Config) pinnedProviders(alias string) []string {
	for _, m := range c.Models {
		if m.ID == alias {
			return append([]string{}, m.Providers...)
		}
	}
	return nil
}

type Attempt struct {
	ErrorKind           string `json:"error_kind,omitempty"`
	UpstreamHTTPStatus  int    `json:"upstream_http_status,omitempty"`
	UpstreamErrorStatus int    `json:"upstream_error_status,omitempty"`
	UpstreamSkipped     bool   `json:"upstream_skipped,omitempty"`
	Status              int    `json:"status"`
	Mode                string `json:"mode"`
	Provider            string `json:"provider"`
	ProviderSource      string `json:"provider_source"`
	DurationMS          int64  `json:"duration_ms"`
	Error               string `json:"error,omitempty"`
}
type LogEntry struct {
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	UpstreamAttempts      *int       `json:"upstream_attempt_count,omitempty"`
	ReasoningEffort       string     `json:"reasoning_effort,omitempty"`
	RequestCount          int64      `json:"request_count,omitempty"`
	LastTime              *time.Time `json:"last_time,omitempty"`
	LastRequestID         string     `json:"last_request_id,omitempty"`
	BackoffWindowID       string     `json:"backoff_window_id,omitempty"`
	OutputTokensPerSecond *float64   `json:"output_tokens_per_second"`
	ErrorKind             string     `json:"error_kind,omitempty"`
	UpstreamHTTPStatus    int        `json:"upstream_http_status,omitempty"`
	UpstreamErrorStatus   int        `json:"upstream_error_status,omitempty"`
	UpstreamSkipped       bool       `json:"upstream_skipped,omitempty"`
	RetryAt               *time.Time `json:"retry_at,omitempty"`
	RateLimitScope        string     `json:"rate_limit_scope,omitempty"`
	promptReported        bool
	completionReported    bool
	estimateKey           string
	CredentialID          string    `json:"credential_id,omitempty"`
	UsageReported         bool      `json:"usage_reported,omitempty"`
	CacheWriteTokens      int64     `json:"cache_write_tokens,omitempty"`
	CacheWriteReported    bool      `json:"cache_write_reported,omitempty"`
	ID                    string    `json:"id"`
	Time                  time.Time `json:"time"`
	Model                 string    `json:"model"`
	UpstreamModel         string    `json:"upstream_model"`
	Stream                bool      `json:"stream"`
	StreamEnd             string    `json:"stream_end,omitempty"`
	Status                int       `json:"status"`
	Provider              string    `json:"provider"`
	ProviderSource        string    `json:"provider_source"`
	DurationMS            int64     `json:"duration_ms"`
	TTFTMS                int64     `json:"ttft_ms"`
	PromptTokens          int64     `json:"prompt_tokens"`
	CompletionTokens      int64     `json:"completion_tokens"`
	CachedTokens          int64     `json:"cached_tokens"`
	ReasoningTokens       int64     `json:"reasoning_tokens"`
	Credential            string    `json:"credential"`
	Attempts              []Attempt `json:"attempts"`
	Error                 string    `json:"error,omitempty"`
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
