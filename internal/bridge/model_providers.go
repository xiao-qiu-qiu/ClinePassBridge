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
var servedByPattern = regexp.MustCompile(`"(resolvedProvider|finalProvider)"\s*:\s*"([^"]+)"`)
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
		Metadata: map[string]any{"egress_management": true},
		deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second),
		Payload:  []byte(`{"messages":[{"role":"user","content":"ok"}],"max_tokens":64}`),
	}
	j, credential, upstream, err := s.prepareProbe(req, in.CredentialID)
	var has any
	var hasPresent bool
	if err == nil {
		has, hasPresent = unpinProviderProbe(j)
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
		stream, err = s.request(req, credential, j, true, entry.UpstreamAttempts, diagnostics)
		if err == nil {
			_, err = s.consumeSSE(stream, in.Model, &entry, &attempt, start, nil)
		}
	}
	duration := time.Since(start).Milliseconds()
	_ = s.observeQuotaError(credential, err)
	finishRequestRateLease(lease, err)
	s.logErrorDetails(&entry, &attempt, err)
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
		note = "上游仅回报本次实际提供方，未列出其他候选"
	default:
		note = "上游未回报提供方或候选，请查看测试结果"
	}
	detail := ""
	if err != nil {
		detail = redactModelTest(safeError(err), credential)
		if diagnostics.body.Len() > 0 {
			detail += "\n\n上游响应 / 传输详情：\n" + redactModelTest(diagnostics.body.String(), credential)
		}
	}
	response := map[string]any{
		"ok": len(candidates) > 0 || served != "", "status": statusOf(err), "error": detail,
		"model": in.Model, "upstream_id": upstream, "duration_ms": duration,
		"credential_id": credential.ID, "credential_label": credential.Label,
		"providers": candidates, "served_by": served, "note": note,
		"probed_at": time.Now().UTC(),
	}
	if hasPresent {
		response["has"] = has
	}
	return managementJSON(200, response)
}

// unpinProviderProbe removes routing hints so discovery can report alternatives,
// while keeping the effective capability filter that selects the route itself.
// Field presence matters: explicit null and [] must survive unchanged.
func unpinProviderProbe(j map[string]any) (any, bool) {
	has, present := object(object(j["providerOptions"])["gateway"])["has"]
	delete(j, "provider")
	delete(j, "providerOptions")
	if present {
		j["providerOptions"] = map[string]any{"gateway": map[string]any{"has": has}}
	}
	return has, present
}

// prepareProbe bypasses only manual disablement for an explicitly selected
// management credential. Automatic selection retains normal request admission.
func (s *Service) prepareProbe(r ExecutorRequest, explicitID string) (map[string]any, Credential, string, error) {
	if explicitID == "" {
		return s.prepare(r)
	}
	c, err := s.probeCredential(explicitID)
	if err != nil {
		return nil, c, "", err
	}
	return s.prepareWithCredential(r, c)
}

// probeCredential resolves an explicitly requested credential even if manually
// disabled, or automatically picks an enabled credential. Both require quota.
func (s *Service) probeCredential(id string) (Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != "" {
		c, ok := s.creds[id]
		if !ok {
			return c, fail(404, "credential not found")
		}
		if c.APIKey == "" || normalizeQuotaCredential(c).QuotaBlock != nil {
			return c, fail(400, "该凭据额度暂停或缺少 API key")
		}
		return c, nil
	}
	candidates := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if c.APIKey != "" && !c.Disabled && normalizeQuotaCredential(c).QuotaBlock == nil {
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
	for _, match := range servedByPattern.FindAllStringSubmatch(text, -1) {
		provider := strings.TrimSpace(match[2])
		if provider == "" {
			continue
		}
		if served == "" || match[1] == "finalProvider" {
			served = provider
		}
		// The final routing decision supersedes a provisional resolvedProvider,
		// even when the latter appears first in a frame or an earlier SSE event.
		if match[1] == "finalProvider" {
			break
		}
	}
	// fallbacksAvailable omits the provider that actually served, so add it
	// back only when the gateway reported alternatives. An empty or missing list
	// alone does not prove that the served provider can be pinned; private routes
	// also report a served provider without alternatives.
	if len(out) > 0 && served != "" && !seen[strings.ToLower(served)] {
		out = append(out, strings.ToLower(served))
	}
	return out, served
}
