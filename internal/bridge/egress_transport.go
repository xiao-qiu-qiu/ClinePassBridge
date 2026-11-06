package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type egressLocalStream struct {
	id    string
	pool  *egressManager
	op    *egressOperation
	lease *egressLease
	body  io.ReadCloser
	once  sync.Once
}

func (stream *egressLocalStream) close() {
	stream.once.Do(func() {
		stream.op.cancel(context.Canceled)
		_ = stream.body.Close()
		stream.lease.release()
		stream.pool.finishOperation(stream.op)
		stream.pool.mu.Lock()
		delete(stream.pool.streams, stream.id)
		stream.pool.mu.Unlock()
	})
}

func (stream *egressLocalStream) read() (readChunk, error) {
	buffer := make([]byte, 32<<10)
	n, err := stream.body.Read(buffer)
	if err == io.EOF {
		return readChunk{Payload: buffer[:n], Done: true}, nil
	}
	if err != nil {
		if stream.op.ctx.Err() != nil {
			return readChunk{Payload: buffer[:n]}, egressContextError(stream.op.ctx)
		}
		return readChunk{Payload: buffer[:n]}, typedTransportFault(stream.op.ctx, err, "egress upstream response interrupted")
	}
	return readChunk{Payload: buffer[:n]}, nil
}

func (p *egressManager) client(proxy string) *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if cached := p.clients[proxy]; cached != nil {
		return cached
	}
	transport := &http.Transport{}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaults.Clone()
	}
	transport.Proxy = nil // Explicit direct never inherits system proxy settings.
	if proxy != "direct" {
		parsed, _ := url.Parse(proxy) // Validated before admission.
		transport.Proxy = http.ProxyURL(parsed)
	}
	// Go supports HTTP, TLS-to-HTTPS-proxy and SOCKS5/SOCKS5H here. Both SOCKS
	// spellings send hostname destinations to the proxy for remote DNS resolution.
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Redirects would be extra, unreserved HTTP requests. Return their response
		// instead of silently sending another request or forwarding a bearer key.
		return http.ErrUseLastResponse
	}}
	p.clients[proxy] = client
	return client
}

func (s *Service) openEgress(payload any, deadline time.Time, diagnostics ...*modelTestDiagnostics) (upstreamStream, bool, error) {
	p := s.getEgressManager()
	// Read the private hooks only from in-process maps. Ordinary host payloads
	// are not reserialized or changed while the pool is disabled.
	values, _ := payload.(map[string]any)
	pin := str(values["egress_proxy_url"])
	management, _ := values["egress_management"].(bool)
	p.mu.Lock()
	enabled := p.cfg.Enabled
	p.mu.Unlock()
	if !enabled && !(management && strings.TrimSpace(pin) != "") {
		return upstreamStream{}, false, nil
	}
	var request struct {
		Callback string      `json:"host_callback_id"`
		Method   string      `json:"method"`
		URL      string      `json:"url"`
		Headers  http.Header `json:"headers"`
		Body     []byte      `json:"body"`
	}
	serializable := payload
	if _, present := values[routingContextKey]; present {
		copied := make(map[string]any, len(values))
		for key, value := range values {
			if key != routingContextKey {
				copied[key] = value
			}
		}
		serializable = copied
	}
	encoded, err := json.Marshal(serializable)
	if err != nil || json.Unmarshal(encoded, &request) != nil {
		return upstreamStream{}, true, fail(400, "invalid egress HTTP request")
	}
	parsed, err := url.Parse(request.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return upstreamStream{}, true, fail(400, "invalid egress target URL")
	}
	if err := s.begin(); err != nil {
		return upstreamStream{}, true, err
	}
	defer s.active.Done()
	parent, _ := values[routingContextKey].(context.Context)
	op, err := p.routingOperation(request.Callback, deadline, parent)
	if err != nil {
		return upstreamStream{}, true, err
	}
	lease, err := p.acquire(op.ctx, pin)
	if err != nil {
		p.finishOperation(op)
		return upstreamStream{}, true, err
	}
	r, err := http.NewRequestWithContext(op.ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		lease.release()
		p.finishOperation(op)
		return upstreamStream{}, true, fail(400, "invalid egress HTTP request")
	}
	r.Header = request.Headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	// Proxy authentication belongs to the configured transport, never the origin.
	r.Header.Del("Proxy-Authorization")
	if err := op.ctx.Err(); err != nil {
		lease.release()
		p.finishOperation(op)
		return upstreamStream{}, true, egressContextError(op.ctx)
	}
	client := p.client(lease.endpoint.ProxyURL)
	if client == nil {
		lease.release()
		p.finishOperation(op)
		return upstreamStream{}, true, bridgeUnavailable("egress transport is closing")
	}
	p.mu.Lock()
	lease.sent = true
	p.mu.Unlock()
	response, err := client.Do(r)
	if err != nil {
		failure := typedTransportFault(op.ctx, err, "egress upstream transport failed; configured proxy was not bypassed")
		if op.ctx.Err() != nil {
			failure = egressContextError(op.ctx)
		}
		lease.release()
		p.finishOperation(op)
		// url.Error and dialer errors can contain proxy credentials. Do not pass
		// their text to diagnostics, request logs, management responses or RPC.
		return upstreamStream{}, true, failure
	}
	local := &egressLocalStream{id: "egress:" + id(), pool: p, op: op, lease: lease, body: response.Body}
	p.mu.Lock()
	if p.closed || op.ctx.Err() != nil {
		failure := bridgeUnavailable("egress transport closed while opening a response")
		if !p.closed && op.ctx.Err() != nil {
			failure = egressContextError(op.ctx)
		}
		p.mu.Unlock()
		local.close()
		return upstreamStream{}, true, failure
	}
	p.streams[local.id] = local
	p.mu.Unlock()
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[local.id] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		local.close()
		return upstreamStream{}, true, bridgeUnavailable("egress transport is closing")
	}
	go func() {
		<-op.ctx.Done()
		s.closeUpstream(local.id)
	}()
	up := upstreamStream{deadline: deadline, StatusCode: response.StatusCode, Headers: response.Header, StreamID: local.id, local: local}
	if len(diagnostics) > 0 {
		up.diagnostics = diagnostics[0]
	}
	return up, true, nil
}

func (s *Service) closeEgressStream(streamID string) bool {
	if !strings.HasPrefix(streamID, "egress:") {
		return false
	}
	p := s.getEgressManager()
	p.mu.Lock()
	stream := p.streams[streamID]
	p.mu.Unlock()
	if stream != nil {
		stream.close()
	}
	s.mu.Lock()
	delete(s.streams, streamID)
	s.mu.Unlock()
	return true
}

// Only classified challenges, HTML 429s or explicit IP/edge codes open an IP window.
// Generic JSON 429s (including monthly/weekly quota) keep their original kind.
func (s *Service) classifyEgressError(up upstreamStream, body map[string]any, raw []byte, original error) error {
	if up.local == nil || up.local.pool == nil || up.local.lease == nil || up.local.lease.group == nil {
		return original
	}
	var detail *upstreamError
	if errors.As(original, &detail) && (detail.Kind == "upstream_quota_exhausted" || detail.Kind == "upstream_team_rate_limited" || detail.Kind == "upstream_auth_error") {
		return original
	}
	if detail != nil && detail.Kind == upstreamEdgeKind {
		p := up.local.pool
		p.mu.Lock()
		now := p.now()
		delay := time.Duration(p.cfg.IPCooldownSeconds) * time.Second
		if retryDelay, ok := egressRetryAfterDelay(up.Headers, now); ok {
			delay = retryDelay
		}
		until := p.cooldownGroupLocked(up.local.lease.group, now.Add(delay))
		p.mu.Unlock()
		result := *detail
		result.RetryAt = until
		return &result
	}
	code := strings.ToLower(str(object(body["error"])["code"]) + " " + str(body["code"]))
	text := strings.ToLower(strings.TrimSpace(string(raw)))
	html := up.StatusCode == 429 && (strings.Contains(strings.ToLower(up.Headers.Get("Content-Type")), "text/html") || strings.HasPrefix(text, "<!doctype html") || strings.HasPrefix(text, "<html"))
	explicit := statusOf(original) == 429 && (strings.Contains(code, "ip_rate_limit") || strings.Contains(code, "edge_rate_limit"))
	if !html && !explicit {
		return original
	}
	p := up.local.pool
	p.mu.Lock()
	now := p.now()
	delay := time.Duration(p.cfg.IPCooldownSeconds) * time.Second
	if up.Headers.Get("Retry-After") != "" || retrySecondsPattern.MatchString(errorMessage(body)) {
		delay = upstreamRetryDelay(up.Headers, errorMessage(body), now)
	}
	until := p.cooldownGroupLocked(up.local.lease.group, now.Add(delay))
	p.mu.Unlock()
	return &upstreamError{APIError: &APIError{429, "upstream_ip_rate_limited", "[clinepassbridge:ip_rate_limit] Upstream edge/IP rate limit"}, HTTPStatus: up.StatusCode, UpstreamStatus: 429, Scope: "egress_ip", RetryAt: until}
}

func (p *egressManager) cooldownGroupLocked(group *egressGroup, until time.Time) time.Time {
	group.cooldown = laterTime(group.cooldown, until)
	p.notifyLocked()
	return group.cooldown
}

// Challenge pages have no provider retry message. Ignore invalid Retry-After
// values so the configured IP cooldown, rather than a generic minute, applies.
func egressRetryAfterDelay(headers http.Header, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	seconds, err := strconv.ParseFloat(value, 64)
	if err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds <= 86400 {
		return max(time.Second, time.Duration(math.Ceil(seconds))*time.Second), true
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) && date.Sub(now) <= 24*time.Hour {
		return date.Sub(now), true
	}
	return 0, false
}
