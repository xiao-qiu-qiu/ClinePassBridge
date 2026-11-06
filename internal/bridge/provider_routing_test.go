package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// capturingHost records the outgoing request bodies so tests can assert on the
// routing fields actually sent upstream, and replies with a minimal completion.
type capturingHost struct {
	mu     sync.Mutex
	bodies []map[string]any
	reads  int
	plan   hostPlan
}

func (c *capturingHost) call(method string, payload, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	request, _ := payload.(map[string]any)
	switch method {
	case "host.http.do_stream":
		var body map[string]any
		if err := json.Unmarshal(request["body"].([]byte), &body); err != nil {
			return err
		}
		c.bodies = append(c.bodies, body)
		*out.(*upstreamStream) = upstreamStream{StatusCode: c.plan.status, Headers: c.plan.header, StreamID: fmt.Sprintf("capture-%d", len(c.bodies))}
		return nil
	case "host.http.stream_read":
		chunks := c.plan.chunks
		if len(chunks) == 0 {
			// Success shorthand: one minimal completion, then EOF.
			chunks = []readChunk{{Payload: simpleSSE()}}
		}
		index := c.reads
		c.reads++
		if index >= len(chunks) {
			*out.(*readChunk) = readChunk{Done: true}
		} else {
			*out.(*readChunk) = chunks[index]
		}
		return nil
	case "host.http.stream_close", "host.stream.emit", "host.stream.close":
		return nil
	default:
		return fmt.Errorf("unexpected host method %q", method)
	}
}

func registerService(t *testing.T, configYAML string) (*Service, error) {
	t.Helper()
	s := NewService()
	_, err := s.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte(configYAML)}))
	return s, err
}

// registerMigratedService opts a fixture into an already migrated settings
// directory. Tests must still select config/client explicitly in their YAML.
// Plain registerService keeps exercising fresh-install migration and defaults.
func registerMigratedService(t *testing.T, configYAML string) (*Service, error) {
	t.Helper()
	var config struct {
		DataDir string `yaml:"data_dir"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &config); err != nil {
		t.Fatalf("decode migrated fixture: %v", err)
	}
	if config.DataDir == "" {
		t.Fatal("migrated fixture requires an explicit data_dir")
	}
	path := filepath.Join(config.DataDir, "settings.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("migrated fixture requires a fresh settings file: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"provider_policy_revision":1}`), 0600); err != nil {
		t.Fatalf("write migrated fixture: %v", err)
	}
	return registerService(t, configYAML)
}

func modelConfig(t *testing.T, extra string) string {
	t.Helper()
	out := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
	out += "models:\n  - id: deepseek-flash\n    upstream_id: cline-pass/deepseek-v4.1-flash\n"
	return out + extra
}

func requestWithPayload(payload string) json.RawMessage {
	return requestForModel("deepseek-flash", payload)
}

func requestForModel(model, payload string) json.RawMessage {
	return jsonBytes(ExecutorRequest{
		Model:          model,
		Payload:        []byte(payload),
		StorageJSON:    jsonBytes(Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}),
		HostCallbackID: "callback-1",
	})
}

// upstreamBodyOf returns the request body actually sent by the executor.
func upstreamBodyOf(t *testing.T, s *Service, payload string) map[string]any {
	t.Helper()
	host := &capturingHost{plan: ssePlan(simpleSSE())}
	s.SetHost(host.call)
	if _, err := s.Handle("executor.execute", requestWithPayload(payload)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.bodies) != 1 {
		t.Fatalf("expected 1 upstream body, got %d", len(host.bodies))
	}
	return host.bodies[0]
}

// gatewayOf also checks that upstreams reading only the top-level provider
// receive the same routing fields, without copying gateway-only options.
func gatewayOf(t *testing.T, s *Service, payload string) map[string]any {
	t.Helper()
	body := upstreamBodyOf(t, s, payload)
	gateway := object(object(body["providerOptions"])["gateway"])
	wantProvider := map[string]any{}
	for _, key := range []string{"only", "order"} {
		if value, ok := gateway[key]; ok {
			wantProvider[key] = value
		}
	}
	if got := object(body["provider"]); string(jsonBytes(got)) != string(jsonBytes(wantProvider)) {
		t.Errorf("provider = %s, want only the resolved routing fields %s", jsonBytes(got), jsonBytes(wantProvider))
	}
	return gateway
}

func slugs(v any) []string {
	out := []string{}
	for _, item := range list(v) {
		out = append(out, str(item))
	}
	return out
}

func TestProviderAllowListIsInjectedAsOnlyAndOrder(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers:\n      - deepseek\n      - wafer\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "deepseek,wafer" {
		t.Errorf("only = %q, want deepseek,wafer", got)
	}
	if got := strings.Join(slugs(gateway["order"]), ","); got != "deepseek,wafer" {
		t.Errorf("order = %q, want deepseek,wafer (list order is the priority order)", got)
	}
}

func TestModelWithClearedProvidersKeepsAutomaticRouting(t *testing.T) {
	for _, upstream := range []string{"cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash-1017"} {
		t.Run(upstream, func(t *testing.T) {
			config := strings.ReplaceAll(modelConfig(t, "    providers: []\nprovider_policy: config\n"), "cline-pass/deepseek-v4.1-flash", upstream)
			s, err := registerMigratedService(t, config)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
			if _, ok := body["provider"]; ok {
				t.Errorf("an unpinned model must not gain legacy provider routing, body = %v", body)
			}
			// Clearing providers removes only/order, independently of the 1017
			// model's capability defaults. Other models still have no options.
			wantOptions := `null`
			if upstream == "cline-pass/deepseek-v4.1-flash-1017" {
				wantOptions = `{"gateway":{"has":["vision","implicit-caching"]}}`
			}
			if got := string(jsonBytes(body["providerOptions"])); got != wantOptions {
				t.Errorf("unpinned providerOptions = %s, want %s", got, wantOptions)
			}
		})
	}
}

func TestClientProviderOverridesConfiguredSetKeyByKey(t *testing.T) {
	// Under client policy the caller wins for the keys it sets, and the
	// configured set fills what it left out.
	s, err := registerMigratedService(t, modelConfig(t, "    providers:\n      - deepseek\n      - wafer\nprovider_policy: client\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"only":["alibaba"]}}}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "alibaba" {
		t.Errorf("only = %q, want the client value alibaba", got)
	}
	if got := strings.Join(slugs(gateway["order"]), ","); got != "deepseek,wafer" {
		t.Errorf("order = %q, want the configured fallback deepseek,wafer", got)
	}
}

func TestTopLevelProviderShorthandIsMirroredIntoGateway(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers: []\nprovider_policy: client\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["novita"],"sort":"latency"}}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "novita" {
		t.Errorf("only = %q, want novita", got)
	}
	if _, ok := gateway["order"]; ok {
		t.Errorf("order must remain absent without a configured fallback, gateway = %v", gateway)
	}
	if gateway["sort"] != "latency" {
		t.Errorf("gateway sort = %v, want latency", gateway["sort"])
	}
}

func TestClientProviderRoutingMergesGatewayAndTopLevelFields(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers: [deepseek, wafer]\nprovider_policy: client\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["novita"],"order":["baseten"],"sort":"latency"},"providerOptions":{"gateway":{"only":["alibaba"],"sort":"price","allowFallbacks":false}}}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "alibaba" {
		t.Errorf("only = %q, want gateway value alibaba", got)
	}
	if got := strings.Join(slugs(gateway["order"]), ","); got != "baseten" {
		t.Errorf("order = %q, want top-level value baseten", got)
	}
	if gateway["sort"] != "price" || gateway["allowFallbacks"] != false {
		t.Errorf("gateway-only options changed: %v", gateway)
	}
}

func TestClientProviderRoutingPreservesExplicitEmptyFields(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers: [deepseek, wafer]\nprovider_policy: client\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["novita"],"order":["novita"]},"providerOptions":{"gateway":{"only":[],"order":null}}}`)
	if got := string(jsonBytes(gateway)); got != `{"only":[],"order":null}` {
		t.Errorf("gateway = %s, want explicit empty fields instead of fallback values", got)
	}
}

func TestConfigPolicyIgnoresClientOverride(t *testing.T) {
	// The configured list decides; a caller's hint is dropped, not an error.
	s, err := registerMigratedService(t, modelConfig(t, "    providers:\n      - deepseek\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["novita"],"order":["baseten"]},"providerOptions":{"gateway":{"only":["alibaba"],"order":["alibaba"]}}}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "deepseek" {
		t.Errorf("only = %q, want the configured deepseek", got)
	}
	if got := strings.Join(slugs(gateway["order"]), ","); got != "deepseek" {
		t.Errorf("order = %q, want the configured deepseek", got)
	}
}

func TestConfigPolicyDropsHintForClearedModel(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers: []\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"provider":{"only":["novita"],"order":["baseten"]},"providerOptions":{"gateway":{"only":["alibaba"],"order":["alibaba"]},"anthropic":{"thinking":{"type":"enabled","budgetTokens":2048}},"deepseek":{"reasoningEffort":"max"}}}`)
	if _, ok := body["provider"]; ok {
		t.Errorf("cleared config must remove top-level routing, body = %v", body)
	}
	opts := object(body["providerOptions"])
	if _, ok := opts["gateway"]; ok {
		t.Errorf("cleared config must remove client gateway routing, options = %v", opts)
	}
	if got := string(jsonBytes(opts)); got != `{"anthropic":{"thinking":{"budgetTokens":2048,"type":"enabled"}},"deepseek":{"reasoningEffort":"max"}}` {
		t.Errorf("other provider options changed: %s", got)
	}
}

func TestConfigPolicyStillInjectsItsOwnSet(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers:\n      - wafer\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "wafer" {
		t.Errorf("only = %q, want wafer", got)
	}
}

func TestProvidersAreNormalisedOnSave(t *testing.T) {
	s, err := registerMigratedService(t, modelConfig(t, "    providers:\n      - \" DeepSeek \"\n      - WAFER\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := strings.Join(slugs(gateway["only"]), ","); got != "deepseek,wafer" {
		t.Errorf("only = %q, want normalised deepseek,wafer", got)
	}
}

func TestProviderConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		extra  string
		reason string
	}{
		{"duplicate", "    providers:\n      - wafer\n      - wafer\n", "unique"},
		{"empty entry", "    providers:\n      - \"\"\n", "empty"},
		{"blank entry", "    providers:\n      - \"a b\"\n", "whitespace"},
		{"too many", "    providers:\n      - a\n      - b\n      - c\n      - d\n      - e\n      - f\n      - g\n      - h\n      - i\n", "at most 8"},
		{"bad policy", "provider_policy: tighten\n", "provider_policy"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registerService(t, modelConfig(t, tt.extra))
			if err == nil {
				t.Fatalf("expected rejection (%s)", tt.reason)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.reason)
			}
		})
	}
}

func TestDeepSeekModelIsPinnedUnderConfigPolicy(t *testing.T) {
	for _, upstream := range []string{"cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4.1-flash-1017"} {
		t.Run(upstream, func(t *testing.T) {
			config := strings.ReplaceAll(modelConfig(t, "provider_policy: config\n"), "cline-pass/deepseek-v4.1-flash", upstream)
			s, err := registerMigratedService(t, config)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
			for _, key := range []string{"only", "order"} {
				if got := strings.Join(slugs(gateway[key]), ","); got != "deepseek" {
					t.Errorf("config policy should use the seeded DeepSeek mapping for %s, got %q", key, got)
				}
			}
		})
	}
}

func TestOtherModelsAreNotPinnedByDefault(t *testing.T) {
	config := fmt.Sprintf("data_dir: %q\n", filepath.ToSlash(t.TempDir()))
	config += "models:\n  - id: glm-flash\n    upstream_id: cline-pass/glm-5.3-flash\n"
	s, err := registerService(t, config)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	host := &capturingHost{plan: ssePlan(simpleSSE())}
	s.SetHost(host.call)
	if _, err := s.Handle("executor.execute", requestForModel("glm-flash", `{"messages":[{"role":"user","content":"hello"}]}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if _, ok := host.bodies[0]["providerOptions"]; ok {
		t.Errorf("only DeepSeek is seeded; body = %v", host.bodies[0])
	}
}

func TestCredentialPriorityNormalisation(t *testing.T) {
	zero, negative, high := 0, -3, 500
	for _, tt := range []struct {
		name  string
		in    *int
		want  int
		isNil bool
	}{
		{"absent", nil, 0, true},
		{"zero", &zero, 0, true},
		{"negative", &negative, 0, true},
		{"value", &high, maxCredentialPriority, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizedPriority(tt.in)
			if tt.isNil {
				if got != nil {
					t.Fatalf("want unset, got %d", *got)
				}
				return
			}
			if got == nil || *got != tt.want {
				t.Fatalf("want %d, got %v", tt.want, got)
			}
		})
	}
}

func TestCredentialsAreOrderedByPriorityThenLabel(t *testing.T) {
	s := NewService()
	five, three := 5, 3
	s.creds = map[string]Credential{
		"id-1": {ID: "id-1", Label: "乙", Priority: &five},
		"id-2": {ID: "id-2", Label: "甲"},
		"id-3": {ID: "id-3", Label: "丙", Priority: &five},
		"id-4": {ID: "id-4", Label: "丁", Priority: &three},
	}
	got := []string{}
	for _, item := range s.credentials() {
		got = append(got, str(item["label"]))
	}
	want := []string{"丙", "乙", "丁", "甲"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (priority desc, then label, unset last)", got, want)
	}
}

func TestRoutingCandidatesExtraction(t *testing.T) {
	// Success frame: snake_case routing block with a candidate list.
	success := `data: {"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":` +
		`{"resolvedProvider":"fireworks","fallbacksAvailable":["fireworks","Alibaba"," deepseek ","fireworks"]}}}}}]}`
	got, served := routingCandidates(success)
	if strings.Join(got, ",") != "fireworks,alibaba,deepseek" {
		t.Errorf("candidates = %v, want normalised and de-duplicated", got)
	}
	if served != "fireworks" {
		t.Errorf("served = %q, want fireworks", served)
	}

	// The served provider is usually absent from the candidate list; it must be
	// added back, since it is a provider this model really can be pinned to.
	omitted := `{"providerMetadata":{"gateway":{"routing":{"resolvedProvider":"baseten",` +
		`"fallbacksAvailable":["fireworks","wafer"]}}}}`
	got, served = routingCandidates(omitted)
	if strings.Join(got, ",") != "fireworks,wafer,baseten" {
		t.Errorf("candidates = %v, want the served provider appended", got)
	}

	// Error frame: camelCase block, no candidate list (a private-pool account).
	// Nothing may be offered here, not even the pseudo-provider that served.
	failure := `{"error":{"message":"failed: {\"providerMetadata\":{\"gateway\":{\"routing\":` +
		`{\"resolvedProvider\":\"openai-compatible-private\"}}}}"}}`
	got, served = routingCandidates(failure)
	if len(got) != 0 {
		t.Errorf("candidates = %v, want none on a private route", got)
	}
	if served != "openai-compatible-private" {
		t.Errorf("served = %q, want openai-compatible-private", served)
	}

	// Nothing to report at all.
	got, served = routingCandidates(`{"error":"empty response content"}`)
	if len(got) != 0 || served != "" {
		t.Errorf("unexpected facts: %v / %q", got, served)
	}
}

func TestMissingProviderPolicyDefaultsToNone(t *testing.T) {
	s, err := registerService(t, modelConfig(t, ""))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := s.config().ProviderPolicy; got != ProviderPolicyNone {
		t.Errorf("provider_policy = %q, want %q", got, ProviderPolicyNone)
	}
}

// errorEntry drives one failing upstream response and returns the recorded log.
func errorEntry(t *testing.T, s *Service, plan hostPlan) LogEntry {
	t.Helper()
	host := &capturingHost{plan: plan}
	s.SetHost(host.call)
	if _, err := s.Handle("executor.execute", requestWithPayload(`{"messages":[{"role":"user","content":"hello"}]}`)); err == nil {
		t.Fatal("expected the request to fail")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.logs) == 0 {
		t.Fatal("no log entry recorded")
	}
	return s.logs[len(s.logs)-1]
}

func jsonErrorPlan(status int, body any) hostPlan {
	return hostPlan{status: status, header: http.Header{"Content-Type": []string{"application/json"}}, chunks: []readChunk{{Payload: jsonBytes(body), Done: true}}}
}

func TestProviderIsRecoveredFromStructuredRoutingBlock(t *testing.T) {
	s, err := registerService(t, modelConfig(t, ""))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	entry := errorEntry(t, s, jsonErrorPlan(429, map[string]any{
		"error":            map[string]any{"message": "ordinary rate limit"},
		"providerMetadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{"resolvedProvider": "fireworks"}}},
	}))
	if entry.Provider != "fireworks" {
		t.Errorf("provider = %q, want fireworks", entry.Provider)
	}
	if !strings.Contains(entry.ProviderSource, "resolvedProvider") {
		t.Errorf("provider_source = %q, want it to name resolvedProvider", entry.ProviderSource)
	}
}

func TestProviderIsRecoveredFromNestedErrorMessage(t *testing.T) {
	// The shape actually observed in production: HTTP 200 with an SSE error
	// frame whose message embeds the routing block as text.
	s, err := registerService(t, modelConfig(t, ""))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	inner := `Failed to create stream: inference request failed: failed to generate stream from Vercel: ` +
		`failed to invoke model 'deepseek/deepseek-v4.1-flash' with streaming: request failed with status 429: ` +
		`{"error":{"message":"Rate limit exceeded for deepseek/deepseek-v4.1-flash: this team's limit of ` +
		`100000000 input tokens per minute (per region) was reached."},"providerMetadata":{"gateway":{"routing":` +
		`{"originalModelId":"deepseek/deepseek-v4.1-flash","resolvedProvider":"baseten"}}}}`
	plan := ssePlan(sseFrame(map[string]any{"error": map[string]any{"code": "stream_initialization_failed", "message": inner}}))
	entry := errorEntry(t, s, plan)
	if entry.Provider != "baseten" {
		t.Errorf("provider = %q, want baseten", entry.Provider)
	}
	if !strings.Contains(entry.ProviderSource, "error.message") {
		t.Errorf("provider_source = %q, want the message fallback", entry.ProviderSource)
	}
	if entry.RateLimitScope != "upstream_team" {
		t.Errorf("rate_limit_scope = %q, want upstream_team", entry.RateLimitScope)
	}
	if entry.UpstreamHTTPStatus != 200 {
		t.Errorf("upstream_http_status = %d, want the transport status 200", entry.UpstreamHTTPStatus)
	}
}

func TestProviderStaysUnknownWhenUpstreamReportsNone(t *testing.T) {
	s, err := registerService(t, modelConfig(t, ""))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	entry := errorEntry(t, s, jsonErrorPlan(500, map[string]any{"error": "empty response content"}))
	if entry.Provider != "unknown" {
		t.Errorf("provider = %q, want unknown", entry.Provider)
	}
}

func TestSuccessfulProviderIsNotOverwritten(t *testing.T) {
	// A locally synthesised rate limit carries no provider; the value already
	// read from the successful frame must survive.
	entry := LogEntry{Provider: "wafer", ProviderSource: "choices[0].delta.provider_metadata.gateway.routing.finalProvider"}
	attempt := Attempt{Provider: "wafer"}
	err := classifyUpstreamError(429, http.Header{}, map[string]any{"error": "rate limit exceeded for deepseek/deepseek-v4.1-flash"}, time.Now())
	logErrorDetails(&entry, &attempt, err)
	if entry.Provider != "wafer" || attempt.Provider != "wafer" {
		t.Errorf("provider = %q/%q, want the earlier value preserved", entry.Provider, attempt.Provider)
	}
}
