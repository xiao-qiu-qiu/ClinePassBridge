package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigFidelityRejectedPUTPreservesStateAndDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "duplicate-providers",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","NOVITA"]}]}`,
		},
		{
			name: "invalid-field-type-after-providers",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","wafer"]}],"log_retention":"invalid"}`,
		},
		{
			name: "malformed-json",
			body: `{"models":[{"id":"deepseek-flash","upstream_id":"cline-pass/deepseek-v4.1-flash","providers":["novita","wafer"]}],`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.DataDir = t.TempDir()
			s.cfg.Models = []Model{{
				ID: "deepseek-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash",
				Providers: []string{"deepseek", "wafer"},
			}}
			path := filepath.Join(s.cfg.DataDir, "settings.json")
			if err := atomicJSON(path, s.cfg); err != nil {
				t.Fatal(err)
			}
			beforeDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Freeze the snapshot as bytes: another config() result could share
			// the same provider backing array and hide a rejected write.
			beforeConfig := jsonBytes(s.config())
			result, err := s.management(jsonBytes(ManagementRequest{
				Method: "PUT", Path: apiBase + "/config", Body: []byte(tc.body),
			}))
			if err != nil {
				t.Fatal(err)
			}
			response, ok := result.(ManagementResponse)
			if !ok {
				t.Fatalf("unexpected management response type %T", result)
			}
			if response.StatusCode != 400 {
				t.Errorf("status = %d, want 400", response.StatusCode)
			}
			if !bytes.Equal(beforeConfig, jsonBytes(s.config())) {
				t.Error("rejected config PUT changed the in-memory configuration")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeDisk, afterDisk) {
				t.Error("rejected config PUT changed settings.json")
			}
		})
	}
}

func TestConfigFidelityCopyPreservesProviderShapeAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
	}{
		{name: "nil", providers: nil},
		{name: "explicit-empty", providers: []string{}},
		{name: "populated", providers: []string{"deepseek", "wafer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.Models = []Model{{ID: "model", UpstreamID: "cline-pass/model", Providers: tc.providers}}
			before := jsonBytes(s.config())
			copied := s.config()
			if !reflect.DeepEqual(copied.Models[0].Providers, tc.providers) {
				t.Error("config copy changed nil, empty or populated provider semantics")
			}
			copied.Models[0].ID = "edited-copy"
			if len(copied.Models[0].Providers) > 0 {
				copied.Models[0].Providers[0] = "novita"
			} else {
				copied.Models[0].Providers = append(copied.Models[0].Providers, "novita")
			}
			if !bytes.Equal(before, jsonBytes(s.config())) {
				t.Error("editing a config copy changed the service configuration")
			}
		})
	}
}

func TestConfigFidelityUnrotatedCredentialSavePreservesUnknown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		clearPriority bool
	}{
		{name: "label-only", body: `{"label":"renamed"}`},
		{name: "clear-priority", body: `{"label":"renamed","priority":0}`, clearPriority: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAffinityFixture(t)
			const credentialID, filename = "stable-0", "migrated-a.json"
			if f.s.creds[credentialID].RoutingID != "" {
				t.Fatal("fixture credential must have an unrotated identity")
			}
			beforeFields := configFidelityCredentialFields(t, f.original[filename])
			var savedPayload []byte
			saveCalls := 0
			f.s.SetHost(func(method string, payload, out any) error {
				if method != "host.auth.save" {
					return fmt.Errorf("unexpected host callback %s", method)
				}
				request, ok := payload.(map[string]any)
				if !ok || request["name"] != filename {
					return fmt.Errorf("credential save used an unexpected filename or payload type")
				}
				raw, ok := request["json"].(json.RawMessage)
				if !ok || !json.Valid(raw) {
					return fmt.Errorf("credential save did not supply valid raw JSON")
				}
				saveCalls++
				savedPayload = append([]byte(nil), raw...)
				// Model the host's full-file save, without decoding through
				// float64 or printing any credential JSON.
				return os.WriteFile(filepath.Join(f.s.authDir, filename), raw, 0600)
			})
			affinityRequest(t, f, ManagementRequest{
				Method: "PUT", Path: apiBase + "/credentials",
				Query: url.Values{"id": {credentialID}}, Body: []byte(tc.body),
			}, 200)
			if saveCalls != 1 {
				t.Fatalf("host.auth.save calls = %d, want 1", saveCalls)
			}
			savedDisk, err := os.ReadFile(filepath.Join(f.s.authDir, filename))
			if err != nil {
				t.Fatal(err)
			}
			changed := []string{"label", "request_scoped_errors"}
			if tc.clearPriority {
				changed = append(changed, "priority")
			}
			for _, saved := range []struct {
				name string
				raw  []byte
			}{
				{name: "host-payload", raw: savedPayload},
				{name: "saved-file", raw: savedDisk},
			} {
				t.Run(saved.name, func(t *testing.T) {
					fields := configFidelityCredentialFields(t, saved.raw)
					if !bytes.Equal(jsonBytes(beforeFields["unknown"]), jsonBytes(fields["unknown"])) {
						t.Error("unknown extension was dropped or its large integer changed")
					}
					assertAffinityFields(t, f.original[filename], saved.raw, changed...)
					if strValue := fields["label"]; !bytes.Equal(strValue, []byte(`"renamed"`)) {
						t.Error("credential label was not updated")
					}
					if _, present := fields["priority"]; tc.clearPriority && present {
						t.Error("cleared priority remained in the saved credential")
					}
				})
			}
			credential := f.s.creds[credentialID]
			if credential.Label != "renamed" || credential.RoutingID != "" {
				t.Error("credential edit did not retain its label and unrotated identity")
			}
			if tc.clearPriority {
				if credential.Priority != nil {
					t.Error("cleared priority remained in memory")
				}
			} else if priorityValue(credential) != 1 {
				t.Error("label-only edit changed credential priority")
			}
			if !bytes.Equal(f.files()["migrated-b.json"], f.original["migrated-b.json"]) {
				t.Error("editing one credential changed another credential file")
			}
		})
	}
}

func configFidelityCredentialFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		t.Fatal("credential fixture is not a JSON object")
	}
	return fields
}
