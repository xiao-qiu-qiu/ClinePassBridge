package bridge

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type upstreamStream struct {
	rejectedBody *bytes.Buffer
	local        *egressLocalStream
	output       *outputStreamLifecycle
	deadline     time.Time
	diagnostics  *modelTestDiagnostics
	callbackID   string
	StatusCode   int               `json:"status_code"`
	Headers      http.Header       `json:"headers"`
	StreamID     string            `json:"stream_id"`
	Error        string            `json:"error,omitempty"`
	ErrorDetails *httpErrorDetails `json:"error_details,omitempty"`
}
type readChunk struct {
	Payload      []byte            `json:"payload"`
	Error        string            `json:"error"`
	ErrorDetails *httpErrorDetails `json:"error_details,omitempty"`
	Done         bool              `json:"done"`
}

func (s *Service) prepare(r ExecutorRequest) (map[string]any, Credential, string, error) {
	c, e := s.selectedCredential(r)
	if e != nil {
		return nil, c, "", e
	}
	return s.prepareWithCredential(r, c)
}

func (s *Service) prepareWithCredential(r ExecutorRequest, c Credential) (map[string]any, Credential, string, error) {
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
	s.applyServerTools(j, r.Model)
	return j, c, up, nil
}

// applyProviderRouting resolves provider preferences once, then writes the
// allow-list (`only`) and priority order (`order`) to both routing forms: some
// upstream request paths read provider, while others read providerOptions.gateway.
func (s *Service) applyProviderRouting(j map[string]any, alias string) {
	cfg := s.config()
	for _, model := range cfg.Models {
		if model.ID == alias && model.RoutingMode == RoutingModeCustom {
			applyCustomProviderRouting(j, model.ProviderOptions, cfg.ProviderPolicy)
			return
		}
	}
	pinned := cfg.pinnedProviders(alias)
	opts := cloneProviderOptions(object(j["providerOptions"]))
	// Merge caller hints before applying the configured policy. Gateway fields
	// take precedence over the top-level shorthand when both are present.
	gateway := map[string]any{}
	for k, v := range object(opts["gateway"]) {
		gateway[k] = v
	}
	for k, v := range object(j["provider"]) {
		if _, ok := gateway[k]; !ok {
			gateway[k] = cloneProviderValue(v)
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
	// Capability filters follow the same rule: a caller that already sent one
	// keeps control, and the configured list only backfills an absent field.
	// Capabilities belong only in providerOptions.gateway, so the top-level
	// provider form below continues to mirror only the provider allow-list/order.
	if _, ok := gateway["has"]; !ok {
		if has := cfg.pinnedHas(alias); len(has) > 0 {
			gateway["has"] = has
		}
	}
	applyOfficialRegion(gateway, cfg, alias)
	// Other provider namespaces in providerOptions must survive, but the
	// gateway sub-key is owned by this function from here on.
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
	// Rebuild the top-level form from the resolved routing fields so caller
	// hints cannot bypass the policy. Gateway-specific options such as sort
	// must not be copied into this form.
	provider := map[string]any{}
	for _, key := range []string{"only", "order"} {
		if value, ok := gateway[key]; ok {
			provider[key] = value
		}
	}
	if len(provider) > 0 {
		j["provider"] = provider
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
func (s *Service) request(r ExecutorRequest, c Credential, j map[string]any, stream bool, attempts *int, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
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
	management, _ := r.Metadata["egress_management"].(bool)
	open := func() (upstreamStream, error) {
		*attempts++
		payload := map[string]any{"host_callback_id": r.HostCallbackID, "method": "POST", "url": s.config().BaseURL + "/chat/completions", "headers": headers(c), "body": jsonBytes(j)}
		return s.openUpstream(s.egressCredentialPayload(payload, c, management), r.deadline, diagnostics...)
	}
	up, err := open()
	opts := object(j["providerOptions"])
	if err != nil || up.StatusCode != http.StatusForbidden || opts["gateway"] == nil {
		return up, err
	}

	// Inspect the rejection before downstream output. Keep the original body
	// for ordinary 403 handling if this is not Cline's routing validation error.
	var rejection bytes.Buffer
	readErr := s.read(up, func(chunk []byte) error {
		_, err := rejection.Write(chunk)
		return err
	})
	up.rejectedBody = &rejection
	body, decodeErr := decodeObject(rejection.Bytes())
	const routingRejected = `Error 403: client-supplied "providerOptions" model routing is not supported`
	if readErr != nil || decodeErr != nil || errorMessage(body) != routingRejected || !time.Now().Before(up.deadline) {
		return up, nil
	}

	delete(j, "provider")
	delete(opts, "gateway")
	if len(opts) == 0 {
		delete(j, "providerOptions")
	}
	r.deadline = up.deadline
	return open()
}

func (s *Service) openUpstream(payload any, deadline time.Time, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	callbackID := ""
	if values, ok := payload.(map[string]any); ok {
		copyPayload := make(map[string]any, len(values)+1)
		for key, value := range values {
			copyPayload[key] = value
		}
		copyPayload["error_details_supported"] = true
		callbackID = str(values["host_callback_id"])
		payload = copyPayload
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	if up, handled, err := s.openEgress(payload, deadline, diagnostics...); handled {
		return up, err
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
		if result.err != nil || out.Error != "" || out.ErrorDetails != nil {
			if result.err == nil && out.Error != "" {
				result.err = errors.New(out.Error)
			}
			failure := hostHTTPFault(result.err, out.ErrorDetails, "upstream transport failed")
			out.diagnostics.transportError(callbackTransportDiagnostic(result.err, out.ErrorDetails, failure))
			s.closeUpstream(out.StreamID)
			return out, failure
		}
	case <-timer.C:
		return out, requestTimedOut("Cline upstream request timed out")
	case <-s.stopCh:
		return out, bridgeUnavailable("ClinePassBridge is shutting down")
	}
	out.deadline = deadline
	out.callbackID = callbackID
	if out.StreamID == "" {
		return out, upstreamProtocolFailure("host returned no upstream stream")
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, bridgeUnavailable("ClinePassBridge is shutting down")
	}
	return out, nil
}
func (s *Service) closeUpstream(id string) {
	if s.closeEgressStream(id) {
		return
	}
	if id != "" {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}
}
func (s *Service) read(up upstreamStream, fn func([]byte) error) error {
	return s.readWatched(up, fn, s.newStreamReadWatchdog(up, 0))
}

func (s *Service) readWatched(up upstreamStream, fn func([]byte) error, watchdog *streamReadWatchdog) (err error) {
	cfg := s.config()
	go watchdog.run()
	defer func() {
		watchdog.stop()
		if err == nil {
			err = watchdog.failure()
		}
		watchdog.close()
	}()
	total := 0
	for {
		var chunk readChunk
		var e error
		if up.local != nil {
			chunk, e = up.local.read()
		} else {
			e = s.call("host.http.stream_read", map[string]any{"stream_id": up.StreamID, "host_callback_id": up.callbackID, "error_details_supported": true}, &chunk)
		}
		if d := watchdog.streamDiagnostics(); d != nil && len(chunk.Payload) > 0 {
			at := time.Now().UTC()
			d.LastUpstreamByteAt = &at
			d.UpstreamReadCount++
		}
		if failure := watchdog.failure(); failure != nil {
			return failure
		}
		total += len(chunk.Payload)
		up.diagnostics.capture(chunk.Payload)
		if total > cfg.MaxResponseBytes {
			return responseLimitFailure("upstream response exceeds configured limit")
		}
		if len(chunk.Payload) > 0 {
			if callbackErr := fn(chunk.Payload); callbackErr != nil {
				return callbackErr
			}
		}
		if failure := watchdog.failure(); failure != nil {
			return failure
		}
		if e != nil || chunk.Error != "" || chunk.ErrorDetails != nil {
			if e == nil && chunk.Error != "" {
				e = errors.New(chunk.Error)
			}
			failure := e
			if up.local == nil || failure == nil {
				failure = hostHTTPFault(e, chunk.ErrorDetails, "upstream stream interrupted")
			}
			up.diagnostics.transportError(callbackTransportDiagnostic(e, chunk.ErrorDetails, failure))
			return failure
		}
		if chunk.Done {
			return nil
		}
	}
}
func (s *Service) readJSON(up upstreamStream) ([]byte, error) {
	return s.readJSONObserved(up, nil)
}

func (s *Service) readJSONObserved(up upstreamStream, observe func(map[string]any)) ([]byte, error) {
	var b bytes.Buffer
	var e error
	if up.rejectedBody != nil {
		b = *up.rejectedBody
	} else {
		e = s.read(up, func(v []byte) error { b.Write(v); return nil })
	}
	var j map[string]any
	if observe != nil || up.StatusCode < 200 || up.StatusCode >= 300 {
		j, _ = decodeObject(b.Bytes())
	}
	if observe != nil && j != nil {
		observe(j)
	}
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		err := classifyUpstreamResponseError(up.StatusCode, up.Headers, j, b.Bytes(), time.Now())
		return nil, s.classifyEgressError(up, j, b.Bytes(), err)
	}
	if e != nil {
		return nil, e
	}
	return b.Bytes(), nil
}
func (s *Service) newLog(r ExecutorRequest, c Credential, up string) LogEntry {
	return LogEntry{DownstreamStarted: new(bool), PromptUsageReported: new(bool), CompletionUsageReported: new(bool), UpstreamAttempts: new(int), estimateKey: s.startEstimateRequest(c), ReasoningEffort: requestReasoningEffort(r.Payload), CredentialID: c.ID, ID: id(), Time: time.Now().UTC(), Model: r.Model, UpstreamModel: up, Stream: r.Stream, Provider: "unknown", ProviderSource: "not_reported", Credential: c.Label, Attempts: []Attempt{}}
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
	s.applyReasoningMapping(j, &entry)
	start := time.Now()
	defer func() {
		_ = s.observeQuotaError(c, e)
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.Status = statusOf(e)
		entry.Error = safeError(e)
		s.logErrorDetails(&entry, nil, e)
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
	defer func() { finishRequestRateLease(lease, e) }()
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
		recordServerTools(j, &entry, &attempt)
		us, e = s.request(r, c, j, wantStream, entry.UpstreamAttempts)
		if e == nil {
			if wantStream {
				var cp *completion
				cp, e = s.consumeSSE(us, r.Model, &entry, &attempt, start, nil, number(j["n"]))
				if e == nil {
					body, e = cp.result(r.Model)
				}
			} else {
				var raw []byte
				raw, e = s.readJSONObserved(us, func(root map[string]any) {
					observeServerToolCalls(root, &entry, &attempt)
				})
				if e == nil {
					var o map[string]any
					root, _ := decodeObject(raw)
					if root["error"] != nil || root["success"] == false {
						e = classifyUpstreamResponseError(us.StatusCode, us.Headers, root, raw, time.Now(), 500)
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
		s.logErrorDetails(&entry, &attempt, e)
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
	observe := func(root map[string]any) { observeServerToolCalls(root, entry, attempt) }
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e := s.readJSONObserved(us, observe)
		return nil, e
	}
	if !strings.Contains(strings.ToLower(us.Headers.Get("Content-Type")), "text/event-stream") {
		_, e := s.readJSONObserved(us, observe)
		if e != nil {
			return nil, e
		}
		return nil, upstreamProtocolFailure("Cline returned non-SSE content for a streaming request")
	}
	cp := newCompletion()
	if len(expectedChoices) > 0 && expectedChoices[0] > 0 {
		cp.expectedChoices = expectedChoices[0]
	}
	cfg := s.config()
	decoder := SSEDecoder{max: cfg.MaxResponseBytes}
	var idleTimeout time.Duration
	if cfg.StreamIdleCutoffEnabled {
		idleTimeout = time.Duration(cfg.StreamIdleTimeoutSeconds) * time.Second
	}
	watchdog := s.newStreamReadWatchdog(us, idleTimeout)
	entry.StreamDiagnostics = nil
	if cfg.Debug {
		entry.StreamDiagnostics = &StreamDiagnostics{IdleTimeoutSeconds: idleTimeout.Seconds()}
	}
	watchdog.diagnostics = entry.StreamDiagnostics
	e := s.readWatched(us, func(b []byte) error {
		return decoder.Feed(b, func(payload []byte, event string) error {
			text := strings.TrimSpace(string(payload))
			if text == "" {
				return nil
			}
			if text == "[DONE]" {
				if d := watchdog.streamDiagnostics(); d != nil {
					d.observeFrame("done")
				}
				cp.done = true
				if !cp.allFinished() {
					return upstreamProtocolFailure("upstream stream ended without finish_reason")
				}
				entry.StreamEnd = "done"
				// CPA owns the downstream SSE envelope and terminal marker.
				return errStreamDone
			}
			j, err := decodeObject(payload)
			if err != nil {
				return upstreamProtocolFailure("upstream returned invalid JSON")
			}
			if j["error"] != nil || event == "error" || j["success"] == false {
				observeServerToolCalls(j, entry, attempt)
				if d := watchdog.streamDiagnostics(); d != nil {
					d.observeFrame("error")
				}
				err := classifyUpstreamResponseError(us.StatusCode, us.Headers, j, payload, time.Now())
				return s.classifyEgressError(us, j, payload, err)
			}
			observeMetadata(j, entry, attempt)
			cp.observe(j)
			if d := watchdog.streamDiagnostics(); d != nil {
				d.observeFrame(streamFrameKind(j))
			}
			if contentStarted(j) {
				if err := watchdog.effectiveOutput(); err != nil {
					return err
				}
				if entry.EffectiveFrameCount == 0 {
					entry.TTFTMS = time.Since(start).Milliseconds()
				}
				entry.EffectiveFrameCount++
				at := time.Now().UTC()
				entry.LastEffectiveFrameAt = &at
			}
			if emit != nil {
				j["model"] = model
				return watchdog.emit(func() error { return emit(jsonBytes(j)) })
			}
			return nil
		})
	}, watchdog)
	if d := watchdog.streamDiagnostics(); d != nil {
		watchdog.mu.Lock()
		if !watchdog.lastOutput.IsZero() {
			d.IdleWaitMS = max(0, time.Since(watchdog.lastOutput).Milliseconds())
		}
		watchdog.mu.Unlock()
	} else {
		entry.StreamDiagnostics = nil
	}
	var upstream *upstreamError
	if errors.As(e, &upstream) && upstream.Kind == upstreamStreamIdleTimeoutKind {
		entry.StreamEnd = "idle_timeout"
	}
	if errors.Is(e, errStreamDone) {
		e = nil
	}
	if e == nil && !cp.done {
		e = decoder.End()
		if e != nil {
			entry.StreamEnd = "eof_incomplete_frame"
			e = upstreamNetworkFailure("eof", "upstream stream ended with an incomplete SSE frame")
		} else if cp.completeAtEOF() {
			cp.done = true
			entry.StreamEnd = "eof_after_finish"
		} else {
			entry.StreamEnd = "eof_incomplete_completion"
			e = upstreamNetworkFailure("eof", fmt.Sprintf("upstream stream closed before [DONE] (choices=%d/%d, finished=%t, output=%t)", len(cp.choices), cp.expectedChoices, cp.allFinished(), cp.hasOutput))
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
	s.applyReasoningMapping(j, &entry)
	start := time.Now()
	var lease *rateLease
	upstreamOpened := false
	attempt := Attempt{Mode: "stream", Provider: "unknown"}
	failEarly := func(err error) (any, error) {
		defer s.active.Done()
		_ = s.observeQuotaError(c, err)
		finishRequestRateLease(lease, err)
		entry.Status = statusOf(err)
		entry.Error = safeError(err)
		entry.DurationMS = time.Since(start).Milliseconds()
		attempt.Status = entry.Status
		attempt.Error = entry.Error
		attempt.DurationMS = entry.DurationMS
		s.logErrorDetails(&entry, &attempt, err)
		entry.Attempts = append(entry.Attempts, attempt)
		if upstreamOpened {
			s.closeOutputStream(r.StreamID, str(r.Metadata["request_path"]), &entry, err)
		}
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
	recordServerTools(j, &entry, &attempt)
	us, e := s.request(r, c, j, true, entry.UpstreamAttempts)
	upstreamOpened = us.StreamID != ""
	if e != nil {
		return failEarly(e)
	}
	if us.StatusCode < 200 || us.StatusCode >= 300 {
		_, e = s.readJSONObserved(us, func(root map[string]any) {
			observeServerToolCalls(root, &entry, &attempt)
		})
		return failEarly(e)
	}
	output := &outputStreamLifecycle{service: s, id: r.StreamID, requestPath: str(r.Metadata["request_path"])}
	us.output = output
	// Preserve the existing RPC handoff separately from acknowledged downstream
	// emits. Errors before the handoff still return through the RPC envelope.
	ready := make(chan error, 1)
	go func() {
		defer s.active.Done()
		attempt.ProviderSource = "not_reported"
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
				return requestCanceled("client disconnected")
			}
			*entry.DownstreamStarted = true
			output.started.Store(true)
			return nil
		}
		defer func() {
			if recover() != nil {
				err = fail(500, "ClinePassBridge stream processing failed")
			}
			if !started && err == nil {
				err = upstreamProtocolFailure("upstream stream ended before any completion frame")
			}
			err = output.close(err, output.phase())
			_ = s.observeQuotaError(c, err)
			attempt.Status = statusOf(err)
			attempt.Error = safeError(err)
			s.logErrorDetails(&entry, &attempt, err)
			attempt.DurationMS = time.Since(start).Milliseconds()
			entry.Attempts = append(entry.Attempts, attempt)
			entry.Status = attempt.Status
			entry.Error = attempt.Error
			entry.DurationMS = attempt.DurationMS
			finishRequestRateLease(lease, err)
			recordOutputStreamClose(&entry, err, output.supported, s.hostFaultPolicy(err))
			s.appendLog(entry)
			if !started {
				ready <- err
			}
		}()
		_, err = s.consumeSSE(us, r.Model, &entry, &attempt, start, func(b []byte) error {
			if !started {
				frame, _ := decodeObject(b)
				if !startsCompletion(frame) {
					pendingBytes += len(b)
					if pendingBytes > 64<<10 {
						return responseLimitFailure("upstream stream prelude exceeds 64 KiB")
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
		if str(choice["finish_reason"]) != "" {
			return true
		}
	}
	return false
}
