package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var secretPattern = regexp.MustCompile(`(?i)(?:bearer\s+|sk[-_])[a-z0-9_.-]+`)

type Service struct {
	authFileMu           sync.Mutex // Never held across host callbacks or watcher waits.
	egress               *egressManager
	configMu             sync.Mutex // Serializes configuration read-modify-save transactions.
	rateLimits           rateLimiter
	credentialMu         sync.Mutex
	authFiles            map[string]string
	mu                   sync.RWMutex
	cfg                  Config
	host                 HostCall
	logs                 []LogEntry
	creds                map[string]Credential
	authDir              string
	loaded               bool
	stopped              bool
	active               sync.WaitGroup
	streams              map[string]struct{}
	revoked              map[string]bool
	stopCh               chan struct{}
	logWriteError        string
	observationTimer     *time.Timer
	logStore             requestLogStore
	statistics           statisticsState
	statisticsLoaded     bool
	statisticsWriteError string
	usageCache           map[string]*usageCacheEntry
	usageSlots           chan struct{}
	usageSamplerStarted  bool
	usageWake            chan struct{}
	modelTests           map[string]bool
	estimates            estimateState
	estimateActivity     map[string]estimateActivity
	estimateWriteError   string
	quotaWriteErrors     map[string]string
	errorRules           []RequestErrorRule
	errorRuleSynced      map[string]string
	errorRuleSyncErrors  map[string]string

	providerPolicyRevision int
}

func NewService() *Service {
	return &Service{cfg: defaultConfig(), creds: map[string]Credential{}, authFiles: map[string]string{}, streams: map[string]struct{}{}, revoked: map[string]bool{}, stopCh: make(chan struct{}), usageCache: map[string]*usageCacheEntry{}, usageSlots: make(chan struct{}, 3), usageWake: make(chan struct{}, 1), errorRules: requestErrorRules(), errorRuleSynced: map[string]string{}, errorRuleSyncErrors: map[string]string{}}
}
func (s *Service) SetHost(h func(string, any, any) error) { s.mu.Lock(); s.host = h; s.mu.Unlock() }
func (s *Service) call(method string, in, out any) error {
	s.mu.RLock()
	h := s.host
	s.mu.RUnlock()
	if h == nil {
		return errors.New("host callback is not initialized")
	}
	return h(method, in, out)
}
func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	if c.ReasoningMapping != nil {
		mapping := c.ReasoningMapping.clone()
		c.ReasoningMapping = &mapping
	}
	c.Models = append([]Model{}, c.Models...)
	for i := range c.Models {
		c.Models[i].ProviderOptions = cloneProviderOptions(c.Models[i].ProviderOptions)
		if c.Models[i].ReasoningMapping != nil {
			mapping := c.Models[i].ReasoningMapping.clone()
			c.Models[i].ReasoningMapping = &mapping
		}
		// JSON decoding and validation mutate these slices. A rejected update
		// must not change live routing; keep nil distinct from an explicit [].
		if c.Models[i].Providers != nil {
			c.Models[i].Providers = append([]string{}, c.Models[i].Providers...)
		}
		if c.Models[i].Has != nil {
			c.Models[i].Has = append([]string{}, c.Models[i].Has...)
		}
	}
	return c
}
func (s *Service) debugEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Debug
}
func id() string { b := make([]byte, 12); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	tmp := path + ".tmp-" + id()
	defer os.Remove(tmp)
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func (s *Service) configure(raw json.RawMessage) (err error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if e := json.Unmarshal(raw, &req); e != nil {
		return e
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if e := yaml.Unmarshal(req.ConfigYAML, &cfg); e != nil {
			return e
		}
	}
	// CPA passes plugin-owned config. State holds UI changes and bounded request metadata, never API keys.
	revision, e := loadProviderPolicySettings(&cfg)
	if e != nil {
		return e
	}
	if e := cfg.validate(); e != nil {
		return e
	}
	s.mu.RLock()
	changingDataDir := s.loaded && filepath.Clean(cfg.DataDir) != filepath.Clean(s.cfg.DataDir)
	s.mu.RUnlock()
	if changingDataDir {
		return fail(409, "Changing data_dir requires restarting the plugin with its complete data directory")
	}
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return e
	}
	rules, err := loadErrorRules(cfg.DataDir)
	if err != nil {
		return err
	}
	revision, err = migrateProviderPolicy(&cfg, revision)
	if err != nil {
		return err
	}
	s.credentialMu.Lock()
	s.authFileMu.Lock()
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		s.authFileMu.Unlock()
		if _, statErr := os.Stat(filepath.Join(cfg.DataDir, errorRulesFile)); err == nil && statErr == nil {
			s.syncErrorRulesLocked()
		}
		s.credentialMu.Unlock()
		if err == nil {
			err = s.reconcileQuotaScheduling()
		}
	}()
	if s.stopped {
		// The host re-registers a quiesced instance when replacement fails.
		// Reload persisted counters rather than reviving an obsolete snapshot.
		s.stopped, s.loaded, s.statisticsLoaded, s.usageSamplerStarted = false, false, false, false
		s.stopCh = make(chan struct{})
		s.logs = nil
		s.egress = newEgressManager()
	}
	s.cfg = cfg
	s.providerPolicyRevision = revision
	s.errorRules = rules
	if !s.loaded {
		if err := s.loadEgressLocked(); err != nil {
			return err
		}
		s.loadEstimatesLocked()
		if e := s.loadRequestLogsLocked(); e != nil {
			return e
		}
		if e := s.loadStatisticsLocked(); e != nil {
			return e
		}
		s.logs = compactBackoffLogs(s.logs)
		s.loaded = true
	}
	if len(s.logs) > cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-cfg.LogRetention:]
		s.queueRequestLogLocked(nil, false)
		s.flushRequestLogsLocked()
	}
	return nil
}
func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if e := s.configure(raw); e != nil {
			return nil, e
		}
		return registration(), nil
	case "executor.identifier", "auth.identifier", "quota.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "quota.describe":
		return map[string]any{"supported_providers": []string{Provider}, "display_name": "Cline Pass", "supports_reset": false}, nil
	case "quota.fetch":
		return s.fetchQuota(raw)
	case "quota.reset":
		return map[string]any{"success": false, "message": "Cline Pass does not support resetting upstream quota"}, nil
	case "model.static", "model.for_auth":
		return s.modelRegistration(), nil
	case "auth.parse":
		return s.parseAuth(raw)
	case "auth.login.start":
		return nil, fail(400, "Import a Cline API key from ClinePassBridge credentials; interactive OAuth is not used")
	case "auth.login.poll":
		return map[string]any{"Status": "error", "Message": "Use API key import"}, nil
	case "auth.refresh":
		return s.refreshAuth(raw)
	case "executor.execute", "executor.execute_stream":
		var r ExecutorRequest
		if e := json.Unmarshal(raw, &r); e != nil {
			return nil, e
		}
		if method == "executor.execute_stream" {
			return s.executeStream(r)
		}
		return s.execute(r)
	case "executor.count_tokens":
		// Stock CPA treats a missing count endpoint as availability-neutral;
		// 501 would cool down an otherwise usable credential/model.
		return nil, &APIError{404, countTokensUnavailableKind, "Cline Pass does not expose an exact token counting endpoint"}
	case "executor.http_request":
		return nil, fail(400, "Use the ClinePassBridge model executor")
	case "management.register":
		return s.registerManagement(raw)
	case "management.handle":
		return s.management(raw)
	case "plugin.quiesce", "plugin.shutdown":
		s.mu.Lock()
		if !s.stopped {
			close(s.stopCh)
		}
		s.stopped = true
		streams := make([]string, 0, len(s.streams))
		for stream := range s.streams {
			streams = append(streams, stream)
		}
		s.mu.Unlock()
		s.getEgressManager().close()
		for _, stream := range streams {
			s.closeUpstream(stream)
		}
		s.active.Wait()
		s.mu.Lock()
		if s.observationTimer != nil {
			s.observationTimer.Stop()
			s.observationTimer = nil
		}
		s.flushObservationsLocked()
		s.mu.Unlock()
		return map[string]any{}, nil
	default:
		return nil, fail(400, "unsupported plugin method: "+method)
	}
}
func (s *Service) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return bridgeUnavailable("ClinePassBridge is shutting down")
	}
	s.active.Add(1)
	return nil
}

// Re-save this provider's auth records so CPA's watcher refreshes model registrations.
func (s *Service) refreshRegistrations() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	revision := sha256.Sum256(jsonBytes(s.config().Models))
	s.mu.RLock()
	credentials := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		credentials = append(credentials, c)
	}
	s.mu.RUnlock()
	for _, c := range credentials {
		baseline := c
		// CPA skips unchanged files. A model revision makes watcher refreshes reliable.
		c.ModelRevision = hex.EncodeToString(revision[:])
		c.RequestScopedErrors = s.currentErrorRules()
		if e := s.saveCredential(c, baseline); e != nil {
			return e
		}
	}
	return nil
}

func registration() any {
	// Static registration would keep models advertised after every account is disabled.
	return map[string]any{"schema_version": 6, "metadata": map[string]any{"Name": "ClinePassBridge", "Version": Version, "Author": "xiao-qiu-qiu", "GitHubRepository": "https://github.com/xiao-qiu-qiu/ClinePassBridge", "Description": "Cline Pass subscription adapter with reliable streaming, usage and actual provider logs", "ConfigFields": []map[string]any{{"Name": "data_dir", "Type": "string", "Description": "Persistent plugin state directory"}}}, "capabilities": map[string]any{"auth_provider": true, "model_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}, "management_api": true, "quota_provider": true}}
}
func (s *Service) modelRegistration() any {
	cfg := s.config()
	models := []map[string]any{}
	for _, m := range cfg.Models {
		models = append(models, map[string]any{"ID": m.ID, "Name": m.UpstreamID, "Object": "model", "OwnedBy": Provider, "DisplayName": m.ID, "SupportedGenerationMethods": []string{"chat"}, "UserDefined": true})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}
func (s *Service) resolveModel(model string) (string, error) {
	cfg := s.config()
	for _, m := range cfg.Models {
		if model == m.ID {
			return m.UpstreamID, nil
		}
	}
	return "", fail(400, "model is not enabled in ClinePassBridge: "+model)
}
func authData(c Credential, filename string) any {
	// Metadata and stored auth JSON must agree, including after hot reload.
	var rules []any
	_ = json.Unmarshal(jsonBytes(c.RequestScopedErrors), &rules)
	c = normalizeQuotaCredential(c)
	return map[string]any{"Provider": Provider, "ID": routingID(c, filename), "FileName": filename, "Label": c.Label, "Disabled": c.Disabled || c.QuotaBlock != nil, "ProxyURL": c.ProxyURL, "StorageJSON": jsonBytes(c), "Metadata": map[string]any{"type": Provider, "request_scoped_errors": rules}, "Attributes": map[string]string{"auth_kind": "api_key"}}
}
func (s *Service) authData(c Credential, filename string) any {
	c.RequestScopedErrors = s.currentErrorRules()
	return authData(c, filename)
}
func (s *Service) parseAuth(raw json.RawMessage) (any, error) {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	var r struct {
		Provider, Path, FileName string
		RawJSON                  []byte
		Host                     struct{ AuthDir string }
	}
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	// Watcher work can arrive after a newer edit. Resolve the physical snapshot
	// again while serialized with plugin file writes, before accepting any fields.
	dir := r.Host.AuthDir
	if dir == "" {
		s.mu.RLock()
		dir = s.authDir
		s.mu.RUnlock()
	}
	resolvedFile := false
	if dir != "" && r.FileName != "" && filepath.Base(r.FileName) == r.FileName && !strings.ContainsAny(r.FileName, `/\`) {
		if latest, err := os.ReadFile(filepath.Join(dir, r.FileName)); err == nil {
			r.RawJSON = latest
			resolvedFile = true
		}
	}
	var c Credential
	if e := json.Unmarshal(r.RawJSON, &c); e != nil || c.Type != Provider {
		return map[string]any{"Handled": false}, nil
	}
	if c.APIKey == "" {
		return nil, fail(401, "Cline Pass credential has no api_key")
	}
	if c.ID == "" {
		c.ID = strings.TrimSuffix(r.FileName, ".json")
	}
	if c.Label == "" {
		c.Label = c.ID
	}
	s.mu.RLock()
	revoked := s.revoked[c.ID]
	s.mu.RUnlock()
	if revoked && !resolvedFile {
		// A watcher may have read RawJSON before deletion and queued its callback.
		// Only a newly present physical credential can reimport a revoked ID.
		return map[string]any{"Handled": false}, nil
	}
	s.applyParsedErrorRules(&c, dir, r.FileName, r.RawJSON)
	staleQuota := !quotaStateEqual(c, normalizeQuotaCredential(c))
	c = normalizeQuotaCredential(c)
	s.mu.Lock()
	if current, exists := s.creds[c.ID]; exists && current.APIKey == c.APIKey && current.QuotaRevision != "" && current.QuotaRevision != c.QuotaRevision {
		// The watcher may have read a file before a quota write (including a failed
		// write). Keep quota-owned state while still accepting manual field edits.
		c.QuotaBlock, c.QuotaIdentity, c.QuotaRevision = current.QuotaBlock, current.QuotaIdentity, current.QuotaRevision
	} else if exists && current.APIKey != c.APIKey {
		delete(s.quotaWriteErrors, c.ID)
	}
	if resolvedFile {
		delete(s.revoked, c.ID)
	}
	s.creds[c.ID] = c
	if staleQuota {
		if s.quotaWriteErrors == nil {
			s.quotaWriteErrors = map[string]string{}
		}
		s.quotaWriteErrors[c.ID] = "额度状态待同步保存"
	}
	if r.FileName != "" {
		s.authFiles[c.ID] = filepath.Base(r.FileName)
	}
	if r.Host.AuthDir != "" {
		s.authDir = r.Host.AuthDir
	}
	s.mu.Unlock()
	s.wakeUsageSampler()
	return map[string]any{"Handled": true, "Auth": s.authData(c, r.FileName)}, nil
}
func (s *Service) refreshAuth(raw json.RawMessage) (any, error) {
	var r struct{ StorageJSON []byte }
	if e := json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	var c Credential
	if e := json.Unmarshal(r.StorageJSON, &c); e != nil {
		return nil, e
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	}
	filename := s.authFiles[c.ID]
	revoked := s.revoked[c.ID]
	s.mu.RUnlock()
	if revoked {
		return nil, fail(401, "Cline Pass credential was removed")
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	return map[string]any{"Auth": s.authData(c, filename), "NextRefreshAfter": time.Now().Add(365 * 24 * time.Hour)}, nil
}
func (s *Service) selectedCredential(r ExecutorRequest) (Credential, error) {
	var c Credential
	if len(r.StorageJSON) > 0 {
		_ = json.Unmarshal(r.StorageJSON, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[r.AuthID]; ok {
		c = current
	} else if c.ID == "" {
		for _, current := range s.creds {
			if routingID(current, s.authFiles[current.ID]) == r.AuthID {
				c = current
				break
			}
		}
	}
	revoked := s.revoked[c.ID] || s.revoked[r.AuthID]
	s.mu.RUnlock()
	if c.APIKey == "" || c.Disabled || revoked {
		return c, fail(401, "Cline Pass credential is missing or disabled")
	}
	if normalizeQuotaCredential(c).QuotaBlock != nil {
		var retryAt time.Time
		if c.QuotaBlock.RetryAt != nil {
			retryAt = *c.QuotaBlock.RetryAt
		}
		return c, &upstreamError{APIError: &APIError{429, "quota_blocked", quotaBlockMarker + " Cline Pass account quota exhausted; waiting for a successful usage check"}, Local: true, Scope: "account", RetryAt: retryAt}
	}
	return c, nil
}
func (s *Service) appendLog(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Consult the live setting: a request may have started before debug was disabled.
	if !s.cfg.Debug {
		entry.StreamDiagnostics = nil
	}
	s.recordEstimateLocked(entry)
	entry.Error = safeError(errors.New(entry.Error))
	completedAt := time.Now().UTC()
	entry.CompletedAt = &completedAt
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	entry.OutputTokensPerSecond = outputTokenRate(entry)
	if entry.UpstreamSkipped {
		entry.Attempts = nil
	}
	s.recordStatisticsLocked(entry)
	var merged bool
	s.logs, merged = mergeBackoffLog(s.logs, entry)
	if len(s.logs) > s.cfg.LogRetention {
		s.logs = s.logs[len(s.logs)-s.cfg.LogRetention:]
	}
	s.queueRequestLogLocked(&s.logs[len(s.logs)-1], merged)
	if merged {
		s.flushRequestLogsLocked()
		s.scheduleObservationsLocked()
	} else {
		s.flushObservationsLocked()
	}
}
func (s *Service) credentials() []map[string]any {
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	s.mu.RUnlock()
	// Highest priority first, then the operator's own label, then the id: a
	// stable, predictable order rather than map iteration order.
	sortCredentials(creds)
	out := []map[string]any{}
	for _, c := range creds {
		entry := map[string]any{"id": c.ID, "label": c.Label, "enabled": !c.Disabled, "priority": c.Priority}
		for key, value := range s.quotaStatus(c.ID) {
			entry[key] = value
		}
		out = append(out, entry)
	}
	return out
}

func priorityValue(c Credential) int {
	if c.Priority == nil {
		return 0
	}
	return *c.Priority
}
