package bridge

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"
)

const providerRoutingRejection = `Error 403: client-supplied "providerOptions" model routing is not supported`

// Keep this context in-process: it is never serialized through the host ABI.
const routingContextKey = "egress_routing_context"

// routingOperation gives each HTTP attempt the same logical cancellation
// parent, without replacing a host's optional callback context binding.
func (p *egressManager) routingOperation(callback string, deadline time.Time, parent context.Context) (*egressOperation, error) {
	if parent == nil {
		return p.operation(callback, deadline)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, bridgeUnavailable("egress transport is closing")
	}
	ctx, deadlineCancel := context.WithDeadline(parent, deadline)
	ctx, cancel := context.WithCancelCause(ctx)
	op := &egressOperation{callback, ctx, cancel, deadlineCancel}
	p.operations[op] = struct{}{}
	return op, nil
}

func (s *Service) requestWithRoutingFallback(r ExecutorRequest, c Credential, j map[string]any, entry *LogEntry, attempt *Attempt, diagnostics ...*modelTestDiagnostics) (upstreamStream, error) {
	if r.deadline.IsZero() {
		r.deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	_, hasOptions := j["providerOptions"]
	management, _ := r.Metadata["egress_management"].(bool)
	var logical *egressOperation
	var pool *egressManager
	if hasOptions && (s.egressEnabled() || management && strings.TrimSpace(c.ProxyURL) != "") {
		pool = s.getEgressManager()
		var err error
		logical, err = pool.operation(r.HostCallbackID, r.deadline)
		if err != nil {
			return upstreamStream{}, err
		}
	}
	retained := false
	defer func() {
		if logical != nil && !retained {
			pool.finishOperation(logical)
		}
	}()
	open := func() (upstreamStream, error) {
		if logical != nil && logical.ctx.Err() != nil {
			return upstreamStream{}, egressContextError(logical.ctx)
		}
		if !time.Now().Before(r.deadline) {
			return upstreamStream{}, requestTimedOut("Cline upstream request timed out")
		}
		*entry.UpstreamAttempts++
		attemptStart := time.Now()
		payload := map[string]any{"host_callback_id": r.HostCallbackID, "method": "POST", "url": s.config().BaseURL + "/chat/completions", "headers": headers(c), "body": jsonBytes(j)}
		payload = s.egressCredentialPayload(payload, c, management)
		if logical != nil {
			payload[routingContextKey] = logical.ctx
		}
		up, err := s.openUpstream(payload, r.deadline, diagnostics...)
		up.attemptStart = attemptStart
		attempt.UpstreamHTTPStatus = up.StatusCode
		return up, err
	}
	keep := func(up upstreamStream) {
		if logical != nil && up.local != nil {
			retained = true
			context.AfterFunc(up.local.op.ctx, func() { pool.finishOperation(logical) })
		}
	}
	started := time.Now()
	up, err := open()
	if err != nil || up.StatusCode != http.StatusForbidden || !hasOptions || entry.RoutingFallback {
		if err == nil {
			keep(up)
		}
		return up, err
	}

	// Read once before emitting anything. Ordinary 403s are replayed to the
	// normal response classifier with their complete body and headers intact.
	var rejection bytes.Buffer
	readErr := s.read(up, func(chunk []byte) error { _, err := rejection.Write(chunk); return err })
	up.rejectedBody = &rejection
	if readErr != nil {
		return up, readErr
	}
	body, decodeErr := decodeObject(rejection.Bytes())
	if decodeErr != nil || errorMessage(body) != providerRoutingRejection {
		return up, nil
	}
	// Closing the first response must not erase a concurrent callback cancel.
	if logical != nil && logical.ctx.Err() != nil {
		return up, egressContextError(logical.ctx)
	}
	if !time.Now().Before(r.deadline) {
		return up, requestTimedOut("Cline upstream request timed out")
	}

	rejected := *attempt
	rejectionErr := classifyUpstreamResponseError(up.StatusCode, up.Headers, body, rejection.Bytes(), time.Now())
	rejected.Status, rejected.Error = statusOf(rejectionErr), safeError(rejectionErr)
	rejected.DurationMS, rejected.RoutingFallback = time.Since(started).Milliseconds(), true
	firstEntry := *entry
	s.logErrorDetails(&firstEntry, &rejected, rejectionErr)
	entry.Attempts = append(entry.Attempts, rejected)
	entry.RoutingFallback = true
	*attempt = Attempt{Mode: attempt.Mode, Provider: "unknown", ProviderSource: "not_reported", RoutingFallback: true}
	delete(j, "providerOptions")
	delete(j, "provider")
	recordServerTools(j, entry, attempt)
	up, err = open()
	if err == nil {
		keep(up)
	}
	return up, err
}
