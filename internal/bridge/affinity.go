package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func sortCredentials(creds []Credential) {
	sort.Slice(creds, func(i, j int) bool {
		left, right := priorityValue(creds[i]), priorityValue(creds[j])
		if left != right {
			return left > right
		}
		if creds[i].Label != creds[j].Label {
			return creds[i].Label < creds[j].Label
		}
		return creds[i].ID < creds[j].ID
	})
}

// Keep the account ID stable for usage and logs. CPA's registration identity
// can change independently to invalidate its explicit-session and LCP caches.
// Before the first rotation, match host.auth.save's filename-based identity.
func routingID(c Credential, filename string) string {
	if c.RoutingID != "" {
		return c.RoutingID
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	name := filepath.Base(filename)
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	return name
}

type affinityFile struct {
	credential Credential
	path       string
	original   json.RawMessage
	updated    map[string]json.RawMessage
}

// Preserve unknown CPA metadata and reject stale credentials before writing.
func (s *Service) affinityFile(c Credential) (affinityFile, error) {
	s.mu.RLock()
	dir, name := s.authDir, s.authFiles[c.ID]
	s.mu.RUnlock()
	if dir == "" || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return affinityFile{}, fmt.Errorf("账号 %s 的凭据文件位置尚未确认", c.Label)
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return affinityFile{}, fmt.Errorf("账号 %s 的凭据文件不可读", c.Label)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return affinityFile{}, fmt.Errorf("读取账号 %s 的凭据文件失败", c.Label)
	}
	var disk Credential
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &disk) != nil || json.Unmarshal(raw, &fields) != nil || disk.Type != Provider ||
		(disk.ID != c.ID && !(disk.ID == "" && strings.TrimSuffix(name, ".json") == c.ID)) ||
		disk.RoutingID != c.RoutingID || disk.APIKey != c.APIKey || disk.Disabled != c.Disabled ||
		priorityValue(disk) != priorityValue(c) || disk.ProxyURL != c.ProxyURL {
		return affinityFile{}, fmt.Errorf("账号 %s 刚有变更，请刷新页面后重试", c.Label)
	}
	return affinityFile{credential: c, path: path, original: raw, updated: fields}, nil
}

// Rotated credentials must only go through the watcher. host.auth.save also
// upserts a generic filename-based ID and would resurrect an obsolete auth.
// Both paths merge into the original file so plugin-unknown fields survive.
func (s *Service) saveCredential(c Credential) error {
	s.mu.RLock()
	current, filename, dir := s.creds[c.ID], s.authFiles[c.ID], s.authDir
	s.mu.RUnlock()
	c.RequestScopedErrors = requestErrorRules()
	body := json.RawMessage(jsonBytes(c))
	if current.RoutingID != "" || (dir != "" && filename != "") {
		file, err := s.affinityFile(current)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(jsonBytes(c), &fields)
		for _, key := range []string{"priority", "proxy_url", "model_revision", "routing_id"} {
			delete(file.updated, key)
		}
		for key, value := range fields {
			file.updated[key] = value
		}
		body = json.RawMessage(jsonBytes(file.updated))
		if current.RoutingID != "" {
			if err := atomicJSON(file.path, file.updated); err != nil {
				return err
			}
		}
	}
	if current.RoutingID == "" {
		if filename == "" {
			filename = c.ID + ".json"
		}
		if err := s.call("host.auth.save", map[string]any{"name": filename, "json": body}, nil); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.creds[c.ID] = c
	s.mu.Unlock()
	return nil
}

type runtimeAuth struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Service) runtimeAuths() ([]runtimeAuth, error) {
	var out struct {
		Files []runtimeAuth `json:"files"`
	}
	err := s.call("host.auth.list", map[string]any{}, &out)
	return out.Files, err
}

// Confirm watcher completion, not traffic. Every file must have exactly one
// new registration, and neither its old routing ID nor legacy ID may remain.
func (s *Service) awaitAffinityRotation(files []affinityFile, next []Credential, started time.Time) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		entries, err := s.runtimeAuths()
		if err != nil {
			return fmt.Errorf("读取 CPA 账号登记状态失败")
		}
		ids := make(map[string]runtimeAuth, len(entries))
		counts := make(map[string]int)
		for _, entry := range entries {
			ids[entry.ID] = entry
			if entry.Provider == Provider {
				counts[entry.Name]++
			}
		}
		ready := true
		for i, c := range next {
			name := filepath.Base(files[i].path)
			newID := routingID(c, name)
			oldID := routingID(files[i].credential, name)
			_, oldExists := ids[oldID]
			oldExists = oldExists && oldID != newID
			_, legacyExists := ids[c.ID]
			legacyExists = legacyExists && c.ID != newID
			entry, exists := ids[newID]
			if oldExists || legacyExists || !exists || entry.Provider != Provider || entry.Name != name || counts[name] != 1 {
				ready = false
			}
			// A generic filename entry may already exist before migration. Its
			// watcher update must actually finish before we rotate it away.
			if files[i].credential.RoutingID == "" && newID == oldID && entry.UpdatedAt.Before(started) {
				ready = false
			}
		}
		if ready {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("CPA 尚未完成账号重新登记，或仍有重复登记")
		case <-s.stopCh:
			return fmt.Errorf("插件正在重新加载")
		case <-tick.C:
		}
	}
}

func (s *Service) clearAffinity(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
	}
	defer s.active.Done()
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if len(r.Body) > 0 {
		var body map[string]json.RawMessage
		if json.Unmarshal(r.Body, &body) != nil || len(body) != 0 {
			return managementJSON(400, map[string]any{"error": "操作已更新为按优先级重新分配会话，请刷新页面后重试"})
		}
	}
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	s.mu.RUnlock()
	if len(creds) == 0 {
		return managementJSON(400, map[string]any{"error": "请先添加账号"})
	}
	sortCredentials(creds)
	if _, err := s.runtimeAuths(); err != nil {
		return managementJSON(503, map[string]any{"error": "读取 CPA 账号登记状态失败，尚未执行重新分配"})
	}
	files := make([]affinityFile, 0, len(creds))
	next := make([]Credential, 0, len(creds))
	for _, c := range creds {
		file, err := s.affinityFile(c)
		if err != nil {
			return managementJSON(409, map[string]any{"error": safeError(err)})
		}
		c.RoutingID = c.ID + "-routing-" + id()
		file.updated["routing_id"] = jsonBytes(c.RoutingID)
		files, next = append(files, file), append(next, c)
	}
	// Keep secrets in auth-dir. The watcher ignores these .bak files; plugin
	// settings, logs and quota data remain free of credential copies.
	backup := filepath.Join(filepath.Dir(files[0].path), ".clinepassbridge-affinity-backups", id())
	if err := os.MkdirAll(backup, 0700); err != nil {
		return managementJSON(500, map[string]any{"error": "保存恢复副本失败，尚未执行重新分配"})
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(backup, filepath.Base(file.path)+".bak"), file.original, 0600); err != nil {
			return managementJSON(500, map[string]any{"error": "保存恢复副本失败，尚未执行重新分配"})
		}
	}
	written := make([][]byte, len(files))
	rollback := func(cause error) (any, error) {
		restored := true
		for i, expected := range written {
			if expected == nil {
				continue
			}
			current, readErr := os.ReadFile(files[i].path)
			if readErr != nil || !bytes.Equal(current, expected) {
				restored = false
				continue // Preserve any external edit made while CPA was refreshing.
			}
			if err := atomicJSON(files[i].path, files[i].original); err != nil {
				restored = false
				continue
			}
			s.mu.Lock()
			s.creds[files[i].credential.ID] = files[i].credential
			s.mu.Unlock()
		}
		message := safeError(cause) + "；已恢复原凭据文件，请稍后刷新重试"
		if !restored {
			message = safeError(cause) + "；部分文件恢复失败，请使用 CPA 凭据目录 .clinepassbridge-affinity-backups 中的恢复副本"
		}
		return managementJSON(500, map[string]any{"error": message})
	}
	writePhase := func(targets []Credential) error {
		for i, file := range files {
			expected := []byte(file.original)
			if written[i] != nil {
				expected = written[i]
			}
			current, err := os.ReadFile(file.path)
			if err != nil || !bytes.Equal(current, expected) {
				return fmt.Errorf("账号 %s 刚有变更，请刷新后重试", file.credential.Label)
			}
			file.updated["routing_id"] = jsonBytes(targets[i].RoutingID)
			if err := atomicJSON(file.path, file.updated); err != nil {
				return fmt.Errorf("保存账号 %s 的新登记失败", file.credential.Label)
			}
			written[i], _ = json.MarshalIndent(file.updated, "", "  ")
			s.mu.Lock()
			s.creds[targets[i].ID] = targets[i]
			s.mu.Unlock()
		}
		return nil
	}
	// Older versions used c.ID in the parser while host.auth.save could also
	// register the filename. On first reset, make the watcher own that filename
	// identity before retiring it, so neither legacy registration can survive.
	canonical := append([]Credential(nil), creds...)
	migrate := false
	for i := range canonical {
		if canonical[i].RoutingID == "" {
			canonical[i].RoutingID = routingID(canonical[i], filepath.Base(files[i].path))
			migrate = true
		}
	}
	if migrate {
		started := time.Now()
		if err := writePhase(canonical); err != nil {
			return rollback(err)
		}
		if err := s.awaitAffinityRotation(files, canonical, started); err != nil {
			return rollback(err)
		}
	}
	if err := writePhase(next); err != nil {
		return rollback(err)
	}
	if err := s.awaitAffinityRotation(files, next, time.Time{}); err != nil {
		return rollback(err)
	}
	return managementJSON(200, map[string]any{
		"reset_count": len(next),
		"message":     "旧会话绑定已清除。下一条请求将按当前优先级重新选择可用账号；正在生成的回复会继续完成。",
	})
}
