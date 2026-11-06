package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Diagnostics are collected only for explicit management probes, never normal traffic.
// The existing response-size limit also bounds diagnostic memory and response size.
type modelTestDiagnostics struct {
	body      bytes.Buffer
	limit     int
	truncated bool
}

func (d *modelTestDiagnostics) capture(b []byte) {
	if d == nil {
		return
	}
	remaining := d.limit - d.body.Len()
	if len(b) > remaining {
		b = b[:remaining]
		d.truncated = true
	}
	d.body.Write(b)
}

func (d *modelTestDiagnostics) transportError(message string) {
	if d != nil {
		d.capture([]byte("\nTransport error: " + message))
	}
}

func redactModelTest(text string, c Credential) string {
	if c.APIKey != "" {
		text = strings.ReplaceAll(text, c.APIKey, "[REDACTED]")
		encoded, _ := json.Marshal(c.APIKey)
		text = strings.ReplaceAll(text, string(encoded[1:len(encoded)-1]), "[REDACTED]")
	}
	return secretPattern.ReplaceAllString(text, "[REDACTED]")
}

func (s *Service) testModel(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	defer s.active.Done()
	var in struct {
		Model           string `json:"model"`
		UpstreamID      string `json:"upstream_id"`
		CredentialID    string `json:"credential_id"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Model == "" || in.CredentialID == "" {
		return managementJSON(400, map[string]any{"error": "请选择模型与测试凭据"})
	}
	in.ReasoningEffort = strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
	switch in.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return managementJSON(400, map[string]any{"error": "无效的测试思考强度"})
	}
	upstream, err := s.resolveModel(in.Model)
	if err != nil {
		return managementJSON(400, map[string]any{"error": safeError(err)})
	}
	if upstream != in.UpstreamID {
		return managementJSON(409, map[string]any{"error": "模型映射已变更，请刷新后重试"})
	}
	key := string(jsonBytes([]string{in.Model, in.CredentialID}))
	s.mu.Lock()
	if s.modelTests[key] || len(s.modelTests) >= 3 {
		s.mu.Unlock()
		return managementJSON(429, map[string]any{"error": "已有模型正在测试，请稍后重试（最多同时 3 个）"})
	}
	if s.modelTests == nil {
		s.modelTests = map[string]bool{}
	}
	s.modelTests[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.modelTests, key); s.mu.Unlock() }()

	cfg := s.config()
	start := time.Now()
	attemptStart := start
	body := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with OK only."}},
		"max_tokens": 64,
	}
	if in.ReasoningEffort != "" {
		body["reasoning_effort"] = in.ReasoningEffort
	}
	req := ExecutorRequest{
		AuthID: in.CredentialID, Model: in.Model, HostCallbackID: r.HostCallbackID,
		Metadata: map[string]any{"egress_management": true},
		deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second),
		Payload:  jsonBytes(body),
	}
	j, credential, upstream, err := s.prepareProbe(req, in.CredentialID)
	if err == nil && upstream != in.UpstreamID {
		err = fail(409, "模型映射已变更，请刷新后重试")
	}
	entry := s.newLog(req, credential, upstream)
	s.applyReasoningMapping(j, &entry)
	attempt := Attempt{Mode: "model-test", Provider: "unknown", ProviderSource: "not_reported"}
	diagnostics := &modelTestDiagnostics{limit: cfg.MaxResponseBytes}
	var lease *rateLease
	if err == nil {
		lease, err = s.rateLimits.acquire(credential, upstream)
	}
	if err == nil {
		var stream upstreamStream
		stream, err = s.request(req, credential, j, true, &entry, &attempt, diagnostics)
		if !stream.attemptStart.IsZero() {
			attemptStart = stream.attemptStart
		}
		if err == nil {
			var completion *completion
			completion, err = s.consumeSSE(stream, in.Model, &entry, &attempt, start, nil)
			if err == nil {
				_, err = completion.result(in.Model)
			}
		}
	}
	duration := time.Since(start).Milliseconds()
	_ = s.observeQuotaError(credential, err)
	finishRequestRateLease(lease, err)
	s.logErrorDetails(&entry, &attempt, err)
	detail := ""
	if err != nil {
		detail = err.Error()
		if diagnostics.body.Len() > 0 {
			detail += "\n\n上游响应 / 传输详情：\n" + diagnostics.body.String()
		}
		if diagnostics.truncated {
			detail += fmt.Sprintf("\n\n[详情超出配置的 %d 字节响应上限，已截断]", cfg.MaxResponseBytes)
		}
		if statusOf(err) == 504 {
			detail += fmt.Sprintf("\n请求超时设置：%d 秒", cfg.TimeoutSeconds)
		}
		detail = redactModelTest(detail, credential)
	}
	attempt.Status, attempt.DurationMS = statusOf(err), time.Since(attemptStart).Milliseconds()
	if err != nil {
		attempt.Error = safeError(fmt.Errorf("%s", redactModelTest(err.Error(), credential)))
	}
	entry.Status, entry.DurationMS, entry.Error = attempt.Status, duration, redactModelTest(attempt.Error, credential)
	entry.Attempts = append(entry.Attempts, attempt)
	s.appendLog(entry)
	// Upstream 401/403 are test results, not failures of CPA management authentication.
	return managementJSON(200, map[string]any{
		"ok": err == nil, "status": statusOf(err), "error": detail, "duration_ms": duration,
		"ttft_ms": entry.TTFTMS, "model": in.Model, "upstream_id": upstream,
		"credential_id": credential.ID, "credential_label": credential.Label,
		"reasoning_effort":  entry.ReasoningEffort,
		"reasoning_mapping": entry.ReasoningMapping,
		"routing_fallback":  entry.RoutingFallback,
		"provider":          entry.Provider, "upstream_skipped": entry.UpstreamSkipped, "tested_at": time.Now().UTC(),
	})
}
