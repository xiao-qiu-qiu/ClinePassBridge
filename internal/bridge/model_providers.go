package bridge

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The gateway reports the providers a model could be routed to inside its
// routing block. Which providers that lists depends on the credential: an
// account on the public gateway lists its candidates, while one diverted to the
// private pool reports none. So this is a probe, not a static table.
var fallbacksPattern = regexp.MustCompile(`"fallbacksAvailable"\s*:\s*\[(.*?)\]`)
var servedByPattern = regexp.MustCompile(`"(?:resolvedProvider|finalProvider)"\s*:\s*"([^"]+)"`)
var quotedPattern = regexp.MustCompile(`"([^"]+)"`)

// discoverProviders probes one model and returns the providers it may be pinned
// to. It sends the smallest possible request and reads the routing block out of
// the raw upstream frames; the reply itself is discarded.
func (s *Service) discoverProviders(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	defer s.active.Done()
	var in struct {
		Model        string `json:"model"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Model == "" {
		return managementJSON(400, map[string]any{"error": "请选择要探测的模型"})
	}
	upstream, err := s.resolveModel(in.Model)
	if err != nil {
		return managementJSON(400, map[string]any{"error": safeError(err)})
	}
	credential, err := s.probeCredential(in.CredentialID)
	if err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	if strings.TrimSpace(credential.ProxyURL) != "" {
		return managementJSON(400, map[string]any{"error": "此 CPA 管理接口暂不支持凭据独立代理的探测，请选择使用全局代理的凭据"})
	}

	key := "providers|" + in.Model + "|" + credential.ID
	s.mu.Lock()
	if s.modelTests[key] || len(s.modelTests) >= 3 {
		s.mu.Unlock()
		return managementJSON(429, map[string]any{"error": "已有探测正在进行，请稍后重试（最多同时 3 个）"})
	}
	if s.modelTests == nil {
		s.modelTests = map[string]bool{}
	}
	s.modelTests[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.modelTests, key); s.mu.Unlock() }()

	cfg := s.config()
	start := time.Now()
	req := ExecutorRequest{
		AuthID: credential.ID, Model: in.Model, HostCallbackID: r.HostCallbackID,
		deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second),
		Payload:  []byte(`{"messages":[{"role":"user","content":"ok"}],"max_tokens":16}`),
	}
	j, credential, upstream, err := s.prepare(req)
	// The probe asks what this model *can* be pinned to, so it must not carry
	// the model's own pin: a request pinned to one provider reports no
	// alternatives, which would make an already-pinned model look unpinnable.
	if err == nil {
		delete(j, "providerOptions")
	}
	entry := s.newLog(req, credential, upstream)
	s.applyReasoningMapping(j, &entry)
	attempt := Attempt{Mode: "provider-probe", Provider: "unknown", ProviderSource: "not_reported"}
	diagnostics := &modelTestDiagnostics{limit: cfg.MaxResponseBytes}
	var lease *rateLease
	if err == nil {
		lease, err = s.rateLimits.acquire(credential, upstream)
	}
	if err == nil {
		var stream upstreamStream
		*entry.UpstreamAttempts++
		stream, err = s.request(req, credential, j, true, diagnostics)
		if err == nil {
			_, err = s.consumeSSE(stream, in.Model, &entry, &attempt, start, nil)
		}
	}
	duration := time.Since(start).Milliseconds()
	lease.finish(err)
	logErrorDetails(&entry, &attempt, err)
	attempt.Status, attempt.DurationMS = statusOf(err), duration
	entry.Status, entry.DurationMS = attempt.Status, duration
	entry.Attempts = []Attempt{attempt}
	s.appendLog(entry)

	// A refusal is as informative as a success here: the routing block is
	// reported either way, so parse whatever came back.
	candidates, served := routingCandidates(diagnostics.body.String())
	note := ""
	switch {
	case len(candidates) > 0:
		note = "上游回报的候选托管方"
	case served != "":
		note = "上游只回报了本次实际使用的托管方，没有候选列表；该账号可能无法钉上游"
	default:
		note = "上游没有回报任何托管方，无法固定"
	}
	detail := ""
	if err != nil {
		detail = redactModelTest(safeError(err), credential)
		if diagnostics.body.Len() > 0 {
			detail += "\n\n上游响应 / 传输详情：\n" + redactModelTest(diagnostics.body.String(), credential)
		}
	}
	return managementJSON(200, map[string]any{
		"ok": len(candidates) > 0 || served != "", "status": statusOf(err), "error": detail,
		"model": in.Model, "upstream_id": upstream, "duration_ms": duration,
		"credential_id": credential.ID, "credential_label": credential.Label,
		"providers": candidates, "served_by": served, "note": note,
		"probed_at": time.Now().UTC(),
	})
}

// probeCredential resolves the credential to probe with: the requested one, or
// the enabled credential the scheduler would prefer.
func (s *Service) probeCredential(id string) (Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != "" {
		c, ok := s.creds[id]
		if !ok {
			return c, fail(404, "credential not found")
		}
		if c.APIKey == "" || c.Disabled {
			return c, fail(400, "该凭据已停用或缺少 API key")
		}
		return c, nil
	}
	candidates := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if c.APIKey != "" && !c.Disabled {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return Credential{}, fail(400, "没有可用的凭据")
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := priorityValue(candidates[i]), priorityValue(candidates[j])
		if left != right {
			return left > right
		}
		if candidates[i].Label != candidates[j].Label {
			return candidates[i].Label < candidates[j].Label
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

// routingCandidates pulls the candidate provider list and the provider that
// served the probe out of the raw upstream frames. Only explicit routing
// statements count.
func routingCandidates(raw string) ([]string, string) {
	// The routing block arrives either inline (SSE frames) or as an escaped JSON
	// string inside an error message. Normalising the escaping lets one set of
	// patterns read both shapes.
	text := strings.ReplaceAll(raw, `\"`, `"`)
	seen := map[string]bool{}
	out := []string{}
	if match := fallbacksPattern.FindStringSubmatch(text); len(match) == 2 {
		for _, item := range quotedPattern.FindAllStringSubmatch(match[1], -1) {
			slug := strings.ToLower(strings.TrimSpace(item[1]))
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			out = append(out, slug)
		}
	}
	served := ""
	if match := servedByPattern.FindStringSubmatch(text); len(match) == 2 {
		served = strings.TrimSpace(match[1])
	}
	// fallbacksAvailable omits the provider that actually served, so add it
	// back — but only when the gateway reported candidates at all. An empty list
	// with a served provider means the account is on a private route where
	// nothing is pinnable, and offering that pseudo-provider would only produce
	// requests that always fail.
	if len(out) > 0 && served != "" && !seen[strings.ToLower(served)] {
		out = append(out, strings.ToLower(served))
	}
	return out, served
}
