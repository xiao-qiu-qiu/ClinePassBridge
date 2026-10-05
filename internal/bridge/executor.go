package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type upstreamStream struct {
	deadline    time.Time
	diagnostics *modelTestDiagnostics
	StatusCode  int         `json:"status_code"`
	Headers     http.Header `json:"headers"`
	StreamID    string      `json:"stream_id"`
}
type readChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func (s *Service) prepare(r ExecutorRequest) (map[string]any, Credential, string, error) {
	c, e := s.selectedCredential(r)
	if e != nil {
		return nil, c, "", e
	}
	up, e := s.resolveModel(r.Model)
	if e != nil {
		return nil, c, "", e
	}
	j, e := decodeObject(r.Payload)
	if e != nil {
		return nil, c, "", fail(400, "invalid request JSON")
	}
	if len(list(j["messages"])) == 0 {
		return nil, c, "", fail(400, "messages must be a nonempty array")
	}
	j["model"] = up
	s.applyProviderRouting(j, r.Model)
	return j, c, up, nil
}

// applyProviderRouting writes the per-model provider allow-list into the
// outgoing body. Cline forwards providerOptions.gateway to Vercel AI Gateway
// untouched, which makes the list an allow-list (`only`) with a priority order
// (`order`) there. Models without a configured list keep the automatic routing
// they had before this existed.
func (s *Service) applyProviderRouting(j map[string]any, alias string) {
	cfg := s.config()
	pinned := cfg.pinnedProviders(alias)
	// Chat Completions accepts a top-level provider shorthand for the same
	// fields. Fold it into providerOptions.gateway so there is one channel to
	// reason about, and so the copy below cannot disagree with itself.
	gateway := map[string]any{}
	for k, v := range object(object(j["providerOptions"])["gateway"]) {
		gateway[k] = v
	}
	for k, v := range object(j["provider"]) {
		if _, ok := gateway[k]; !ok {
			gateway[k] = v
		}
	}
	delete(j, "provider")
	if cfg.ProviderPolicy == ProviderPolicyConfig {
		// The configured list decides. A caller's hint is dropped rather than
		// failing the request over something it cannot influence.
		gateway = map[string]any{}
	}
	// The caller wins key by key; the configured list fills what it left out.
	if _, ok := gateway["only"]; !ok && len(pinned) > 0 {
		gateway["only"] = pinned
	}
	if _, ok := gateway["order"]; !ok && len(pinned) > 0 {
		gateway["order"] = pinned
	}
	// Other provider namespaces in providerOptions must survive, but the
	// gateway sub-key is owned by this function from here on.
	opts := object(j["providerOptions"])
	if opts != nil {
		delete(opts, "gateway")
	}
	if len(gateway) == 0 {
		if len(opts) == 0 {
			delete(j, "providerOptions")
		} else {
			j["providerOptions"] = opts
		}
		return
	}
	if opts == nil {
		opts = map[string]any{}
	}
	opts["gateway"] = gateway
	j["providerOptions"] = opts
}
func headers(c Credential) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + c.APIKey}, "Content-Type": []string{"application/json"}, "User-Agent": []string{"ClinePassBridge/" + Version}}
}
func (s *Service) request(r ExecutorRequest, c Credential, j map[string]any, stream bool, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	j["stream"] = stream
	if stream {
		opts := object(j["stream_options"])
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		j["stream_options"] = opts
	} else {
		delete(j, "stream_options")
	}
	return s.openUpstream(map[string]any{"host_callback_id": r.HostCallbackID, "method": "POST", "url": s.config().BaseURL + "/chat/completions", "headers": headers(c), "body": jsonBytes(j)}, r.deadline, diagnostics...)
}

func (s *Service) openUpstream(payload any, deadline time.Time, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	type result struct {
		up  upstreamStream
		err error
	}
	results := make(chan result)
	abandoned := make(chan struct{})
	defer close(abandoned)
	if err := s.begin(); err != nil {
		return upstreamStream{}, err
	}
	go func() {
		defer s.active.Done()
		var up upstreamStream
		err := s.call("host.http.do_stream", payload, &up)
		select {
		case results <- result{up, err}:
		case <-abandoned:
			s.closeUpstream(up.StreamID)
		}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var out upstreamStream
	select {
	case result := <-results:
		out = result.up
		if len(diagnostics) > 0 {
			out.diagnostics = diagnostics[0]
		}
		if result.err != nil {
			out.diagnostics.transportError(result.err.Error())
			s.closeUpstream(out.StreamID)
			return out, fail(502, "upstream transport failed: "+safeError(result.err))
		}
	case <-timer.C:
		return out, fail(504, "Cline upstream request timed out")
	case <-s.stopCh:
		return out, fail(503, "ClinePassBridge is shutting down")
	}
	out.deadline = deadline
	if out.StreamID == "" {
		return out, fail(502, "host returned no upstream stream")
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, fail(503, "ClinePassBridge is shutting down")
	}
	return out, nil
}
func (s *Service) closeUpstream(id string) {
	if id != "" {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}
}
func (s *Service) read(up upstreamStream, fn func([]byte) error) error {
	cfg := s.config()
	var timedOut bool
	var mu sync.Mutex
	deadline := up.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	}
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() { defer close(timerDone); mu.Lock(); timedOut = true; mu.Unlock(); s.closeUpstream(up.StreamID) })
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
	}()
	defer s.closeUpstream(up.StreamID)
	total := 0
	for {
		var chunk readChunk
		e := s.call("host.http.stream_read", map[string]any{"stream_id": up.StreamID}, &chunk)
		mu.Lock()
		timeout := timedOut
		mu.Unlock()
		if timeout {
			return fail(504, "Cline upstream request timed out")
		}
		if e != nil {
			up.diagnostics.transportError(e.Error())
			return fail(502, "upstream read failed: "+safeError(e))
		}
		if chunk.Error != "" {
			up.diagnostics.transportError(chunk.Error)
			return fail(502, "upstream stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		total += len(chunk.Payload)
		up.diagnostics.capture(chunk.Payload)
		if total > cfg.MaxResponseBytes {
			return fail(502, "upstream response exceeds configured limit")
		}
		if len(chunk.Payload) > 0 {
			if e = fn(chunk.Payload); e != nil {
				return e
			}
		}
		if chunk.Done {
			return nil
		}
	}
}
func (s *Service) readJSON(up upstreamStream) ([]byte, error) {
	var b bytes.Buffer
	e := s.read(up, func(v []byte) error { b.Write(v); return nil })
	if e != nil {
		return nil, e
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		j, _ := decodeObject(b.Bytes())
		return nil, classifyUpstreamError(up.StatusCode, up.Headers, j, time.Now())
	}
	return b.Bytes(), nil
}
func (s *Service) newLog(r ExecutorRequest, c Credential, up string) LogEntry {
	return LogEntry{UpstreamAttempts: new(int), estimateKey: s.startEstimateRequest(c), ReasoningEffort: requestReasoningEffort(r.Payload), CredentialID: c.ID, ID: id(), Time: time.Now().UTC(), Model: r.Model, UpstreamModel: up, Stream: r.Stream, Provider: "unknown", ProviderSource: "not_reported", Credential: c.Label, Attempts: []Attempt{}}
}
func (s *Service) execute(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	r.Stream = false
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	start := time.Now()
	defer func() {
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Status = statusOf(e)
		entry.Error = safeError(e)
		logErrorDetails(&entry, nil, e)
		s.appendLog(entry)
	}()
	if e != nil {
		return nil, e
	}
	var lease *rateLease
	lease, e = s.rateLimits.acquire(c, up)
	if e != nil {
		return nil, e
	}
	defer func() { lease.finish(e) }()
	cfg := s.config()
	wantStream := cfg.NonstreamMode == "stream-aggregate"
	var body []byte
	for n := 0; n < 2; n++ {
		attempt := Attempt{Provider: "unknown", ProviderSource: "not_reported", Mode: "nonstream"}
		if wantStream {
			attempt.Mode = "stream-aggregate"
		}
		t := time.Now()
		var us upstreamStream
		*entry.UpstreamAttempts++
		us, e = s.request(r, c, j, wantStream)
		if e == nil {
			if wantStream {
				var cp *completion
				cp, e = s.consumeSSE(us, r.Model, &entry, &attempt, start, nil, number(j["n"]))
				if e == nil {
					body, e = cp.result(r.Model)
				}
			} else {
				var raw []byte
				raw, e = s.readJSON(us)
				if e == nil {
					var o map[string]any
					root, _ := decodeObject(raw)
					if root["error"] != nil || root["success"] == false {
						e = classifyUpstreamError(us.StatusCode, us.Headers, root, time.Now(), 500)
					} else {
						o, e = unwrap(raw)
					}
					if e == nil {
						observeMetadata(o, &entry, &attempt)
						o["model"] = r.Model
						body = jsonBytes(o)
					}
				}
			}
		}
		attempt.Status = statusOf(e)
		attempt.Error = safeError(e)
		logErrorDetails(&entry, &attempt, e)
		attempt.DurationMS = time.Since(t).Milliseconds()
		entry.Attempts = append(entry.Attempts, attempt)
		if e == nil {
			break
		}
		if n == 0 && !wantStream && cfg.NonstreamMode == "native-fallback" && isEmptyError(e) {
			wantStream = true
			continue
		}
		break
	}
	if e != nil {
		return nil, e
	}
	return Response{Payload: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}
func (s *Service) consumeSSE(us upstreamStream, model string, entry *LogEntry, attempt *Attempt, start time.Time, emit func([]byte) error, expectedChoices ...int64) (*completion, error) {
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e := s.readJSON(us)
		return nil, e
	}
	if !strings.Contains(strings.ToLower(us.Headers.Get("Content-Type")), "text/event-stream") {
		_, e := s.readJSON(us)
		if e != nil {
			return nil, e
		}
		return nil, fail(502, "Cline returned non-SSE content for a streaming request")
	}
	cp := newCompletion()
	if len(expectedChoices) > 0 && expectedChoices[0] > 0 {
		cp.expectedChoices = expectedChoices[0]
	}
	decoder := SSEDecoder{max: s.config().MaxResponseBytes}
	e := s.read(us, func(b []byte) error {
		return decoder.Feed(b, func(payload []byte, event string) error {
			if strings.TrimSpace(string(payload)) == "[DONE]" {
				cp.done = true
				if !cp.allFinished() {
					return fail(502, "upstream stream ended without finish_reason")
				}
				entry.StreamEnd = "done"
				// CPA owns the downstream SSE envelope and terminal marker.
				return errStreamDone
			}
			j, err := decodeObject(payload)
			if err != nil {
				return err
			}
			if j["error"] != nil || event == "error" || j["success"] == false {
				return classifyUpstreamError(us.StatusCode, us.Headers, j, time.Now())
			}
			observeMetadata(j, entry, attempt)
			cp.observe(j)
			if entry.TTFTMS == 0 && contentStarted(j) {
				entry.TTFTMS = time.Since(start).Milliseconds()
			}
			if emit != nil {
				j["model"] = model
				return emit(jsonBytes(j))
			}
			return nil
		})
	})
	if errors.Is(e, errStreamDone) {
		e = nil
	}
	if e == nil && !cp.done {
		e = decoder.End()
		if e != nil {
			entry.StreamEnd = "eof_incomplete_frame"
		} else if cp.completeAtEOF() {
			cp.done = true
			entry.StreamEnd = "eof_after_finish"
		} else {
			entry.StreamEnd = "eof_incomplete_completion"
			e = fail(502, fmt.Sprintf("upstream stream closed before [DONE] (choices=%d/%d, finished=%t, output=%t)", len(cp.choices), cp.expectedChoices, cp.allFinished(), cp.hasOutput))
		}
	}
	return cp, e
}
func (s *Service) executeStream(r ExecutorRequest) (any, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	r.Stream = true
	r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	j, c, up, e := s.prepare(r)
	entry := s.newLog(r, c, up)
	start := time.Now()
	var lease *rateLease
	failEarly := func(err error) (any, error) {
		defer s.active.Done()
		lease.finish(err)
		entry.Status = statusOf(err)
		entry.Error = safeError(err)
		entry.DurationMS = time.Since(start).Milliseconds()
		attempt := Attempt{Status: entry.Status, Error: entry.Error, Mode: "stream", Provider: "unknown", DurationMS: entry.DurationMS}
		logErrorDetails(&entry, &attempt, err)
		entry.Attempts = append(entry.Attempts, attempt)
		s.appendLog(entry)
		return nil, err
	}
	if e != nil {
		return failEarly(e)
	}
	if r.StreamID == "" {
		return failEarly(fail(500, "host provided no output stream identifier"))
	}
	lease, e = s.rateLimits.acquire(c, up)
	if e != nil {
		return failEarly(e)
	}
	*entry.UpstreamAttempts++
	us, e := s.request(r, c, j, true)
	if e != nil {
		return failEarly(e)
	}
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e = s.readJSON(us)
		return failEarly(e)
	}
	// Until actual output arrives, return errors through the structured RPC
	// envelope. host.stream.close only carries text and loses HTTP status.
	ready := make(chan error, 1)
	go func() {
		defer s.active.Done()
		attempt := Attempt{Mode: "stream", Provider: "unknown", ProviderSource: "not_reported"}
		var err error
		started := false
		var pending [][]byte
		pendingBytes := 0
		emit := func(b []byte) error {
			// Preserve CPA's native Chat/Responses and Claude framing contracts.
			if str(r.Metadata["request_path"]) == "/v1/messages" {
				b = append(append([]byte("data: "), b...), []byte("\n\n")...)
			}
			if e := s.call("host.stream.emit", map[string]any{"stream_id": r.StreamID, "payload": b}, nil); e != nil {
				return fail(499, "client disconnected")
			}
			return nil
		}
		defer func() {
			if recover() != nil {
				err = fail(500, "ClinePassBridge stream processing failed")
			}
			if !started && err == nil {
				err = fail(502, "upstream stream ended before any completion frame")
			}
			attempt.Status = statusOf(err)
			attempt.Error = safeError(err)
			logErrorDetails(&entry, &attempt, err)
			attempt.DurationMS = time.Since(start).Milliseconds()
			entry.Attempts = append(entry.Attempts, attempt)
			entry.Status = attempt.Status
			entry.Error = attempt.Error
			entry.DurationMS = attempt.DurationMS
			lease.finish(err)
			s.appendLog(entry)
			if !started {
				ready <- err
			} else {
				_ = s.call("host.stream.close", map[string]any{"stream_id": r.StreamID, "error": safeError(err)}, nil)
			}
		}()
		_, err = s.consumeSSE(us, r.Model, &entry, &attempt, start, func(b []byte) error {
			if !started {
				frame, _ := decodeObject(b)
				if !startsCompletion(frame) {
					pendingBytes += len(b)
					if pendingBytes > 64<<10 {
						return fail(502, "upstream stream prelude exceeds 64 KiB")
					}
					pending = append(pending, b)
					return nil
				}
				started = true
				lease.finish(nil) // The single recovery probe reached model output.
				ready <- nil
				for _, frame := range pending {
					if e := emit(frame); e != nil {
						return e
					}
				}
				pending = nil
			}
			return emit(b)
		}, number(j["n"]))
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}}}, nil
}

func startsCompletion(frame map[string]any) bool {
	if contentStarted(frame) {
		return true
	}
	for _, raw := range list(frame["choices"]) {
		choice := object(raw)
		delta := object(choice["delta"])
		if str(delta["refusal"]) != "" || len(object(delta["function_call"])) > 0 || str(choice["finish_reason"]) != "" {
			return true
		}
	}
	return false
}
