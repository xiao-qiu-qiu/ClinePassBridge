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
		Model        string `json:"model"`
		UpstreamID   string `json:"upstream_id"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Model == "" || in.CredentialID == "" {
		return managementJSON(400, map[string]any{"error": "请选择模型与测试凭据"})
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
	req := ExecutorRequest{
		AuthID: in.CredentialID, Model: in.Model, HostCallbackID: r.HostCallbackID,
		deadline: start.Add(time.Duration(cfg.TimeoutSeconds) * time.Second),
		Payload:  []byte(`{"messages":[{"role":"user","content":"Reply with OK only."}],"max_tokens":64}`),
	}
	j, credential, upstream, err := s.prepare(req)
	if err == nil && upstream != in.UpstreamID {
		err = fail(409, "模型映射已变更，请刷新后重试")
	}
	entry := s.newLog(req, credential, upstream)
	attempt := Attempt{Mode: "model-test", Provider: "unknown", ProviderSource: "not_reported"}
	diagnostics := &modelTestDiagnostics{limit: cfg.MaxResponseBytes}
	// CPA's management callback supports only the global proxy, not auth-specific proxies.
	if err == nil && strings.TrimSpace(credential.ProxyURL) != "" {
		err = fail(400, "此 CPA 管理接口暂不支持凭据独立代理的模型测试，请选择使用全局代理的凭据")
	}
	if err == nil {
		var stream upstreamStream
		stream, err = s.request(req, credential, j, true, diagnostics)
		if err == nil {
			var completion *completion
			completion, err = s.consumeSSE(stream, in.Model, &entry, &attempt, start, nil)
			if err == nil {
				_, err = completion.result(in.Model)
			}
		}
	}
	duration := time.Since(start).Milliseconds()
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
	attempt.Status, attempt.DurationMS = statusOf(err), duration
	if err != nil {
		attempt.Error = safeError(fmt.Errorf("%s", redactModelTest(err.Error(), credential)))
	}
	entry.Status, entry.DurationMS, entry.Error = attempt.Status, duration, redactModelTest(attempt.Error, credential)
	entry.Attempts = []Attempt{attempt}
	s.appendLog(entry)
	// Upstream 401/403 are test results, not failures of CPA management authentication.
	return managementJSON(200, map[string]any{
		"ok": err == nil, "status": statusOf(err), "error": detail, "duration_ms": duration,
		"ttft_ms": entry.TTFTMS, "model": in.Model, "upstream_id": upstream,
		"credential_id": credential.ID, "credential_label": credential.Label,
		"provider": entry.Provider, "tested_at": time.Now().UTC(),
	})
}
