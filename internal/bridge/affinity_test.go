package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type affinityFixture struct {
	s        *Service
	t        *testing.T
	original map[string][]byte
	calls    int
	last     []runtimeAuth
	hook     func([]runtimeAuth) ([]runtimeAuth, error)
}

func newAffinityFixture(t *testing.T) *affinityFixture {
	t.Helper()
	s := NewService()
	root := t.TempDir()
	s.authDir, s.cfg.DataDir = filepath.Join(root, "auth"), filepath.Join(root, "state")
	if err := os.MkdirAll(s.authDir, 0700); err != nil {
		t.Fatal(err)
	}
	f := &affinityFixture{s: s, t: t, original: map[string][]byte{}}
	for i, name := range []string{"migrated-a.json", "migrated-b.json"} {
		// Stable IDs differ from generic filename IDs; one account is disabled.
		fields := map[string]any{
			"type": Provider, "id": fmt.Sprintf("stable-%d", i), "label": fmt.Sprintf("account-%d", i),
			"api_key": fmt.Sprintf("fixture-key-%d", i), "priority": i + 1, "disabled": i == 1,
			"unknown": json.RawMessage(`{"serial":9007199254740993,"nested":[null,{"enabled":true}]}`),
		}
		raw, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		if err := os.WriteFile(filepath.Join(s.authDir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		f.original[name] = raw
	}
	entries, err := f.watch()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ID != entry.Name {
			t.Fatalf("initial runtime ID = %q, want filename %q", entry.ID, entry.Name)
		}
	}
	s.SetHost(f.host)
	return f
}

func (f *affinityFixture) files() map[string][]byte {
	f.t.Helper()
	files := make(map[string][]byte)
	for name := range f.original {
		raw, err := os.ReadFile(filepath.Join(f.s.authDir, name))
		if err != nil {
			f.t.Fatal(err)
		}
		files[name] = raw
	}
	return files
}

func (f *affinityFixture) watch() ([]runtimeAuth, error) {
	files, err := os.ReadDir(f.s.authDir)
	if err != nil {
		return nil, err
	}
	var entries []runtimeAuth
	for _, file := range files {
		// CPA recursively ignores .bak files, including private recovery copies.
		if file.IsDir() && file.Name() == ".clinepassbridge-affinity-backups" {
			continue
		}
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			return nil, fmt.Errorf("unexpected watched file %q", file.Name())
		}
		raw, err := os.ReadFile(filepath.Join(f.s.authDir, file.Name()))
		if err != nil {
			return nil, err
		}
		parsed, err := f.s.parseAuth(jsonBytes(map[string]any{
			"Provider": Provider, "FileName": file.Name(), "RawJSON": raw,
			"Host": map[string]any{"AuthDir": f.s.authDir},
		}))
		if err != nil {
			return nil, err
		}
		auth := parsed.(map[string]any)["Auth"].(map[string]any)
		entries = append(entries, runtimeAuth{ID: auth["ID"].(string), Name: auth["FileName"].(string), Provider: auth["Provider"].(string), UpdatedAt: time.Now()})
	}
	return entries, nil
}

func (f *affinityFixture) host(method string, payload, out any) error {
	if method != "host.auth.list" {
		// Detect even ignored host.auth.save errors or attempted upstream traffic.
		f.t.Errorf("unexpected host callback %q", method)
		return fmt.Errorf("unexpected host callback %q", method)
	}
	f.calls++
	entries, err := f.watch()
	if err == nil && f.hook != nil {
		entries, err = f.hook(entries)
	}
	if err != nil {
		return err
	}
	f.last = append([]runtimeAuth(nil), entries...)
	return json.Unmarshal(jsonBytes(map[string]any{"files": entries}), out)
}

func affinityRequest(t *testing.T, f *affinityFixture, request ManagementRequest, status int) ManagementResponse {
	t.Helper()
	out, err := f.s.management(jsonBytes(request))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := out.(ManagementResponse)
	if !ok || response.StatusCode != status {
		t.Fatalf("response = %#v, want status %d", out, status)
	}
	return response
}

func affinityReset(t *testing.T, f *affinityFixture, status int) ManagementResponse {
	t.Helper()
	return affinityRequest(t, f, ManagementRequest{Method: "POST", Path: apiBase + "/credentials/clear-affinity"}, status)
}

func assertAffinityFields(t *testing.T, before, after []byte, changed ...string) {
	t.Helper()
	var left, right map[string]json.RawMessage
	if err := json.Unmarshal(before, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &right); err != nil {
		t.Fatal(err)
	}
	for _, key := range changed {
		delete(left, key)
		delete(right, key)
	}
	// RawMessage preserves large unknown integers; compare without whitespace.
	if !bytes.Equal(jsonBytes(left), jsonBytes(right)) {
		t.Error("unexpected credential field change")
	}
}

func assertAffinityRuntime(t *testing.T, f *affinityFixture, want map[string]string) {
	t.Helper()
	entries, err := f.s.runtimeAuths()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		if entry.Provider != Provider || got[entry.Name] != "" {
			t.Fatalf("duplicate or unexpected registration: %+v", entries)
		}
		got[entry.Name] = entry.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime registrations = %v, want %v", got, want)
	}
}

func assertAffinityBackup(t *testing.T, f *affinityFixture) {
	t.Helper()
	root := filepath.Join(f.s.authDir, ".clinepassbridge-affinity-backups")
	dirs, err := os.ReadDir(root)
	if err != nil || len(dirs) != 1 {
		t.Fatalf("backup directories = %d, err = %v, want one", len(dirs), err)
	}
	dir := filepath.Join(root, dirs[0].Name())
	paths := map[string]os.FileMode{root: 0700, dir: 0700}
	for name, original := range f.original {
		path := filepath.Join(dir, name+".bak")
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatalf("backup %q differs from original: %v", name, err)
		}
		paths[path] = 0600
	}
	if runtime.GOOS != "windows" {
		for path, mode := range paths {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("backup %q is not private (want %o): %v", path, mode, err)
			}
		}
	}
	if _, err := os.Stat(f.s.cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("reset wrote credential copies into plugin state: %v", err)
	}
}

func TestClearAffinityRotatesWithoutTrafficAndWaitsForWatcher(t *testing.T) {
	f := newAffinityFixture(t)
	beforeCreds := jsonBytes(f.s.creds)
	stalePolls := 0
	canonicalPolls := 0
	f.hook = func(entries []runtimeAuth) ([]runtimeAuth, error) {
		if f.s.creds["stable-0"].RoutingID == "migrated-a.json" && canonicalPolls < 2 {
			if canonicalPolls == 0 {
				// A preexisting generic registration is not a watcher acknowledgement.
				for i := range entries {
					entries[i].UpdatedAt = time.Time{}
				}
			} else {
				entries = append(entries, runtimeAuth{ID: "stable-0", Name: "migrated-a.json", Provider: Provider, UpdatedAt: time.Now()})
			}
			canonicalPolls++
		} else if strings.Contains(f.s.creds["stable-0"].RoutingID, "-routing-") && stalePolls < 3 {
			// Require disappearance of old and legacy IDs and any extra auth per file.
			staleIDs := []string{"migrated-a.json", "stable-0", "duplicate-auth"}
			entries = append(entries, runtimeAuth{ID: staleIDs[stalePolls], Name: "migrated-a.json", Provider: Provider})
			stalePolls++
		}
		return entries, nil
	}
	seen := map[string]bool{"migrated-a.json": true, "migrated-b.json": true, "stable-0": true, "stable-1": true}
	for rotation := 0; rotation < 2; rotation++ {
		response := affinityReset(t, f, 200)
		var result struct {
			Count int `json:"reset_count"`
		}
		if err := json.Unmarshal(response.Body, &result); err != nil || result.Count != 2 {
			t.Fatalf("reset response = %s, err = %v", response.Body, err)
		}
		if canonicalPolls != 2 || stalePolls != 3 || len(f.last) != len(f.original) {
			t.Fatal("reset returned while obsolete or duplicate registrations remained")
		}
		want := map[string]string{}
		for name, raw := range f.files() {
			assertAffinityFields(t, f.original[name], raw, "routing_id")
			var c Credential
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if c.RoutingID == "" || seen[c.RoutingID] {
				t.Fatalf("routing ID reused: %q", c.RoutingID)
			}
			seen[c.RoutingID], want[name] = true, c.RoutingID
		}
		assertAffinityRuntime(t, f, want)
		stable := map[string]Credential{}
		for stableID, c := range f.s.creds {
			c.RoutingID = ""
			stable[stableID] = c
		}
		if !bytes.Equal(beforeCreds, jsonBytes(stable)) || len(f.s.logs) != 0 {
			t.Fatal("stable credentials changed or reset required traffic")
		}
		if rotation == 0 {
			assertAffinityBackup(t, f)
		}
	}
}

func TestClearAffinityRejectsBeforeWriting(t *testing.T) {
	for _, tt := range []struct {
		name      string
		body      []byte
		hostError bool
		status    int
	}{
		{"host state unreadable", nil, true, 503},
		{"old hold parameter", []byte(`{"hold_seconds":7}`), false, 400},
		{"old credential parameter", []byte(`{"credential_id":"stable-0"}`), false, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAffinityFixture(t)
			before := jsonBytes(f.s.creds)
			if tt.hostError {
				f.hook = func([]runtimeAuth) ([]runtimeAuth, error) { return nil, fmt.Errorf("host state unreadable") }
			}
			affinityRequest(t, f, ManagementRequest{Method: "POST", Path: apiBase + "/credentials/clear-affinity", Body: tt.body}, tt.status)
			for name, raw := range f.files() {
				if !bytes.Equal(raw, f.original[name]) {
					t.Errorf("rejected request rewrote %s", name)
				}
			}
			if !bytes.Equal(before, jsonBytes(f.s.creds)) {
				t.Error("rejected request changed memory")
			}
			wantCalls := 0
			if tt.hostError {
				wantCalls = 1
			}
			if f.calls != wantCalls {
				t.Fatalf("host list calls = %d, want %d", f.calls, wantCalls)
			}
			if _, err := os.Stat(filepath.Join(f.s.authDir, ".clinepassbridge-affinity-backups")); !os.IsNotExist(err) {
				t.Fatalf("rejected request created backups: %v", err)
			}
		})
	}
}

func TestClearAffinityHostErrorRollsBack(t *testing.T) {
	f := newAffinityFixture(t)
	before := jsonBytes(f.s.creds)
	injected := false
	f.hook = func(entries []runtimeAuth) ([]runtimeAuth, error) {
		if !injected && strings.Contains(f.s.creds["stable-0"].RoutingID, "-routing-") {
			for _, entry := range entries {
				if !strings.Contains(entry.ID, "-routing-") {
					t.Fatal("failure injected before credential writes")
				}
			}
			injected = true
			return nil, fmt.Errorf("post-write host error")
		}
		return entries, nil
	}
	affinityReset(t, f, 500)
	if !injected {
		t.Fatal("post-rotation host failure was not exercised")
	}
	for name, raw := range f.files() {
		assertAffinityFields(t, f.original[name], raw)
	}
	if !bytes.Equal(before, jsonBytes(f.s.creds)) {
		t.Error("rollback left rotated credentials in memory")
	}
	assertAffinityBackup(t, f)
	assertAffinityRuntime(t, f, map[string]string{"migrated-a.json": "migrated-a.json", "migrated-b.json": "migrated-b.json"})
}

func TestClearAffinityRollbackPreservesExternalEdit(t *testing.T) {
	f := newAffinityFixture(t)
	var external []byte
	f.hook = func(entries []runtimeAuth) ([]runtimeAuth, error) {
		if len(external) == 0 && strings.Contains(f.s.creds["stable-0"].RoutingID, "-routing-") {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(f.files()["migrated-a.json"], &fields); err != nil {
				t.Fatal(err)
			}
			fields["unknown"] = json.RawMessage(`{"external_edit":true}`)
			external = jsonBytes(fields)
			if err := os.WriteFile(filepath.Join(f.s.authDir, "migrated-a.json"), external, 0600); err != nil {
				t.Fatal(err)
			}
			return nil, fmt.Errorf("host error concurrent with external edit")
		}
		return entries, nil
	}
	response := affinityReset(t, f, 500)
	files := f.files()
	if len(external) == 0 || !bytes.Equal(files["migrated-a.json"], external) {
		t.Fatal("rollback overwrote the external edit")
	}
	assertAffinityFields(t, f.original["migrated-b.json"], files["migrated-b.json"])
	if !bytes.Contains(response.Body, []byte(".clinepassbridge-affinity-backups")) {
		t.Fatal("partial rollback response omitted recovery location")
	}
	assertAffinityBackup(t, f)
}

func TestRotatedCredentialSaveAndModelRefreshKeepRoutingIdentity(t *testing.T) {
	f := newAffinityFixture(t)
	affinityReset(t, f, 200)
	want := map[string]string{}
	for stableID, c := range f.s.creds {
		want[f.s.authFiles[stableID]] = c.RoutingID
	}
	affinityRequest(t, f, ManagementRequest{Method: "PUT", Path: apiBase + "/credentials", Query: url.Values{"id": {"stable-0"}}, Body: []byte(`{"label":"renamed","priority":0}`)}, 200)
	afterSave := f.files()
	for name, raw := range afterSave {
		assertAffinityFields(t, f.original[name], raw, "routing_id", "label", "priority", "request_scoped_errors")
	}
	if c := f.s.creds["stable-0"]; c.Label != "renamed" || c.Priority != nil {
		t.Fatal("credential edit was not applied")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(afterSave["migrated-a.json"], &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["priority"]; exists {
		t.Fatal("cleared priority remained on disk")
	}
	refreshed, err := f.s.refreshAuth(jsonBytes(map[string]any{"StorageJSON": f.original["migrated-a.json"]}))
	if err != nil {
		t.Fatal(err)
	}
	if auth := refreshed.(map[string]any)["Auth"].(map[string]any); auth["ID"] != want["migrated-a.json"] {
		t.Fatal("stale auth refresh revived a generic identity")
	}
	assertAffinityRuntime(t, f, want)

	// Only model-catalog refresh gets mocked HTTP; reset and save get none.
	catalogRequests := 0
	f.s.SetHost(func(method string, payload, out any) error {
		switch method {
		case "host.http.do_stream":
			request := payload.(map[string]any)
			if request["method"] != "GET" || request["url"] != f.s.config().BaseURL+"/ai/cline/recommended-models" {
				t.Errorf("unexpected catalog request: %v", request)
			}
			catalogRequests++
			*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "catalog", Headers: http.Header{"Content-Type": {"application/json"}}}
		case "host.http.stream_read":
			*out.(*readChunk) = readChunk{Payload: []byte(`{"clinePass":[{"id":"cline-pass/fixture-model"}]}`), Done: true}
		case "host.http.stream_close":
			return nil
		default:
			return f.host(method, payload, out)
		}
		return nil
	})
	models, err := f.s.refreshModels("catalog-callback")
	if err != nil || len(models) != 1 || catalogRequests != 1 {
		t.Fatalf("catalog refresh: models=%v requests=%d err=%v", models, catalogRequests, err)
	}
	// PUT /models invokes refreshRegistrations, the path that actually saves auth.
	affinityRequest(t, f, ManagementRequest{Method: "PUT", Path: apiBase + "/models", Body: jsonBytes(map[string]any{"models": models})}, 200)
	for name, raw := range f.files() {
		assertAffinityFields(t, afterSave[name], raw, "model_revision", "request_scoped_errors")
	}
	for _, c := range f.s.creds {
		if c.ModelRevision == "" {
			t.Fatal("model save did not refresh credential registration")
		}
	}
	assertAffinityRuntime(t, f, want)
}
