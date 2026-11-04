package bridge

import "encoding/json"

func (s *Service) saveOfficialRouting(r ManagementRequest) (any, error) {
	var in struct {
		Model      string `json:"model"`
		UpstreamID string `json:"upstream_id"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(r.Body, &in); err != nil || in.Enabled == nil {
		return managementJSON(400, map[string]any{"error": "invalid official routing settings"})
	}
	cfg := s.config()
	for i := range cfg.Models {
		if cfg.Models[i].ID != in.Model {
			continue
		}
		if cfg.Models[i].UpstreamID != in.UpstreamID {
			return managementJSON(409, map[string]any{"error": "模型已变更，请刷新后重试"})
		}
		cfg.Models[i].OfficialRouting = *in.Enabled
		if err := s.saveConfig(cfg); err != nil {
			return managementJSON(statusOf(err), map[string]any{"error": safeError(err)})
		}
		return managementJSON(200, map[string]any{"models": cfg.Models})
	}
	return managementJSON(404, map[string]any{"error": "模型不存在，请刷新后重试"})
}
