package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExecutorPreservesRequestNumbers(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(ssePlan(simpleSSE()))
			bodies := make(chan []byte, 1)
			s.SetHost(func(method string, payload, out any) error {
				if method == "host.http.do_stream" {
					bodies <- payload.(map[string]any)["body"].([]byte)
				}
				return h.call(method, payload, out)
			})
			var req ExecutorRequest
			if err := json.Unmarshal(executorRequest("number-fidelity"), &req); err != nil {
				t.Fatal(err)
			}
			req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}],"seed":9007199254740993,"providerOptions":{"custom":{"id":9223372036854775807}},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","const":9007199254740993}}}}}]}`)
			if _, err := s.Handle(method, jsonBytes(req)); err != nil {
				t.Fatal(err)
			}
			if method == "executor.execute_stream" {
				select {
				case <-h.clientClosed:
				case <-time.After(3 * time.Second):
					t.Fatal("stream did not finish")
				}
			}
			var got struct {
				Seed    json.RawMessage `json:"seed"`
				Options struct {
					Custom struct {
						ID json.RawMessage `json:"id"`
					} `json:"custom"`
				} `json:"providerOptions"`
				Tools []struct {
					Function struct {
						Parameters struct {
							Properties struct {
								ID struct {
									Const json.RawMessage `json:"const"`
								} `json:"id"`
							} `json:"properties"`
						} `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(<-bodies, &got); err != nil {
				t.Fatal(err)
			}
			if string(got.Seed) != "9007199254740993" || string(got.Options.Custom.ID) != "9223372036854775807" || len(got.Tools) != 1 || string(got.Tools[0].Function.Parameters.Properties.ID.Const) != "9007199254740993" {
				t.Fatalf("request numbers were changed: seed=%s options=%s tools=%+v", got.Seed, got.Options.Custom.ID, got.Tools)
			}
		})
	}
}

func TestDecodeObjectRejectsTrailingJSON(t *testing.T) {
	for _, input := range []string{`{} {}`, `{} true`, `{} invalid`, `null`, `[]`} {
		if _, err := decodeObject([]byte(input)); err == nil {
			t.Errorf("accepted invalid object %q", input)
		}
	}
}

func TestDecodedUsageNumberFormats(t *testing.T) {
	for _, input := range []string{`{"tokens":1000}`, `{"tokens":1000.0}`, `{"tokens":1e3}`} {
		j, err := decodeObject([]byte(input))
		if err != nil || number(j["tokens"]) != 1000 {
			t.Errorf("token count changed for %s: %v, %v", input, j, err)
		}
	}
}
