package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func usageTestService(t *testing.T, respond func(map[string]any) (int, string)) *Service {
	t.Helper()
	s := registeredService(t, "")
	s.creds["one"] = Credential{ID: "one", Type: Provider, APIKey: "secret-test-key"}
	var mu sync.Mutex
	streams := map[string]string{}
	next := 0
	s.SetHost(func(method string, payload, out any) error {
		req := payload.(map[string]any)
		switch method {
		case "host.http.do_stream":
			if req["method"] != "GET" || req["host_callback_id"] != "usage-callback" {
				return fmt.Errorf("wrong method/context")
			}
			status, body := respond(req)
			mu.Lock()
			next++
			id := fmt.Sprint(next)
			streams[id] = body
			mu.Unlock()
			*out.(*upstreamStream) = upstreamStream{StatusCode: status, StreamID: id}
		case "host.http.stream_read":
			mu.Lock()
			body := streams[str(req["stream_id"])]
			mu.Unlock()
			*out.(*readChunk) = readChunk{Payload: []byte(body), Done: true}
		case "host.http.stream_close":
			mu.Lock()
			delete(streams, str(req["stream_id"]))
			mu.Unlock()
		default:
			return fmt.Errorf("unexpected callback %s", method)
		}
		return nil
	})
	return s
}

func usageTestRequest() ManagementRequest {
	return ManagementRequest{Method: "GET", Path: apiBase + "/credentials/usage", Query: url.Values{"id": {"one"}}, HostCallbackID: "usage-callback"}
}

const testLimits = `{"success":true,"data":{"limits":[{"type":"five_hour","percentUsed":0,"resetsAt":"2026-09-25T11:21:35.329131025Z"},{"type":"weekly","percentUsed":11},{"type":"future_window","percentUsed":101.5}]}}`
const testPlan = `{"success":true,"data":{"userId":"private-user","subscriptionId":"private-subscription","currentPeriodEnd":"2026-10-22T14:08:10Z","plan":{"displayName":"Cline Pass (Monthly)"}}}`

func TestCredentialUsageCacheErrorsAndKeyReplacement(t *testing.T) {
	calls, status := 0, 200
	s := usageTestService(t, func(req map[string]any) (int, string) {
		calls++
		key := req["headers"].(http.Header).Get("Authorization")
		if key != "Bearer secret-test-key" && key != "Bearer replacement-key" {
			t.Errorf("credential header missing")
		}
		if status != 200 {
			return status, `{"error":{"message":"secret-test-key private-error"}}`
		}
		if strings.HasSuffix(str(req["url"]), "/usage-limits") {
			return 200, testLimits
		}
		return 200, testPlan
	})
	req := usageTestRequest()
	raw, err := s.management(jsonBytes(req))
	if err != nil {
		t.Fatal(err)
	}
	response := raw.(ManagementResponse)
	var first credentialUsage
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &first) != nil || first.Status != "ok" || first.Plan.Name != "Cline Pass (Monthly)" || len(first.Limits) != 3 || *first.Limits[0].PercentUsed != 0 || first.Limits[0].ResetsAt == nil {
		t.Fatalf("bad usage response: %s", response.Body)
	}
	for _, secret := range []string{"secret-test-key", "private-user", "private-subscription"} {
		if strings.Contains(string(response.Body), secret) {
			t.Fatal("private field exposed")
		}
	}
	req.Query.Set("refresh", "1")
	s.credentialUsage(req)
	if calls != 2 {
		t.Fatalf("refresh ignored short cache: %d", calls)
	}
	past := time.Now().Add(-2 * time.Minute)
	s.usageCache["one"].value.CheckedAt = &past
	status = 429
	failed, _ := s.credentialUsage(req)
	if failed.Status != "rate_limited" || failed.UpdatedAt == nil || !failed.UpdatedAt.Equal(*first.UpdatedAt) || len(failed.Limits) != 3 || calls != 3 || strings.Contains(failed.Error, "secret") {
		t.Fatalf("lost stale data or leaked upstream error: %+v", failed)
	}
	status = 200
	c := s.creds["one"]
	c.APIKey = "replacement-key"
	s.creds["one"] = c
	replaced, _ := s.credentialUsage(req)
	if replaced.Status != "ok" || calls != 5 {
		t.Fatalf("old key cache reused: %+v, calls %d", replaced, calls)
	}
	c.Disabled = true
	s.creds["one"] = c
	disabled, _ := s.credentialUsage(req)
	if disabled.Status != "disabled" || calls != 5 {
		t.Fatal("disabled credential queried")
	}
	c.Disabled = false
	c.ProxyURL = "http://private-proxy"
	s.creds["one"] = c
	proxy, _ := s.credentialUsage(req)
	if proxy.Status != "proxy_unsupported" || calls != 5 {
		t.Fatal("credential proxy silently bypassed")
	}
}

func TestCredentialUsageConcurrentReaders(t *testing.T) {
	var calls atomic.Int32
	s := usageTestService(t, func(req map[string]any) (int, string) {
		calls.Add(1)
		if strings.HasSuffix(str(req["url"]), "/usage-limits") {
			return 200, testLimits
		}
		return 200, testPlan
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := s.credentialUsage(usageTestRequest())
			if err != nil || value.Status != "ok" {
				t.Errorf("concurrent query: %v %s", err, value.Status)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("duplicate upstream fetches: %d", calls.Load())
	}
}

func TestCredentialUsageRejectsOldInFlightKey(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := usageTestService(t, func(req map[string]any) (int, string) {
		if strings.HasSuffix(str(req["url"]), "/usage-limits") {
			if req["headers"].(http.Header).Get("Authorization") == "Bearer secret-test-key" {
				close(started)
				<-release
			}
			return 200, testLimits
		}
		return 200, testPlan
	})
	done := make(chan error, 1)
	go func() { _, err := s.credentialUsage(usageTestRequest()); done <- err }()
	<-started
	s.mu.Lock()
	c := s.creds["one"]
	c.APIKey = "replacement-key"
	s.creds["one"] = c
	s.mu.Unlock()
	fresh, err := s.credentialUsage(usageTestRequest())
	if err != nil || fresh.Status != "ok" {
		t.Fatalf("replacement query failed: %v", err)
	}
	close(release)
	if err := <-done; statusOf(err) != 409 {
		t.Fatalf("old credential response accepted: %v", err)
	}
	current, _ := s.credentialUsage(usageTestRequest())
	if !current.UpdatedAt.Equal(*fresh.UpdatedAt) {
		t.Fatal("old in-flight query overwrote new cache")
	}
}

func TestCredentialUsageMissingFieldsAndPartialPlanFailure(t *testing.T) {
	for _, tc := range []struct {
		name, limits, plan string
		want               string
		planError          bool
	}{
		{"missing limits", `{"success":true,"data":{}}`, testPlan, "unavailable", false},
		{"empty limits no plan", `{"success":true,"data":{"limits":[]}}`, `{"success":true,"data":null}`, "ok", false},
		{"plan failure keeps limits", testLimits, `{"success":false}`, "ok", true},
		{"unknown percent", `{"success":true,"data":{"limits":[{"type":"weekly","resetsAt":"invalid"}]}}`, testPlan, "ok", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := usageTestService(t, func(req map[string]any) (int, string) {
				if strings.HasSuffix(str(req["url"]), "/usage-limits") {
					return 200, tc.limits
				}
				return 200, tc.plan
			})
			value, err := s.credentialUsage(usageTestRequest())
			if err != nil || value.Status != tc.want || (value.PlanError != "") != tc.planError {
				t.Fatalf("unexpected response: %+v, %v", value, err)
			}
			if tc.name == "unknown percent" && (value.Limits[0].PercentUsed != nil || value.Limits[0].ResetsAt != nil) {
				t.Fatal("missing usage fabricated as zero")
			}
		})
	}
}
