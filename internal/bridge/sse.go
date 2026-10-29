package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SSEDecoder tolerates arbitrary network chunk boundaries, CRLF and multiline data.
type SSEDecoder struct {
	buffer []byte
	data   []string
	size   int
	done   bool
	max    int
	event  string
}

func (d *SSEDecoder) Feed(b []byte, emit func([]byte, string) error) error {
	d.buffer = append(d.buffer, b...)
	if len(d.buffer) > d.max {
		return responseLimitFailure("SSE frame exceeds configured limit")
	}
	for {
		i := bytes.IndexByte(d.buffer, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSuffix(string(d.buffer[:i]), "\r")
		d.buffer = d.buffer[i+1:]
		if line == "" {
			if len(d.data) > 0 {
				payload := []byte(strings.Join(d.data, "\n"))
				if e := emit(payload, d.event); e != nil {
					return e
				}
			}
			d.data = nil
			d.size = 0
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "data:") {
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			d.data = append(d.data, v)
			d.size += len(v)
			if d.size > d.max {
				return responseLimitFailure("SSE frame exceeds configured limit")
			}
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	return nil
}
func (d *SSEDecoder) End() error {
	if strings.TrimSpace(string(d.buffer)) != "" || len(d.data) > 0 {
		return upstreamProtocolFailure("upstream stream ended with an incomplete SSE frame")
	}
	return nil
}

type completion struct {
	root            map[string]any
	choices         map[int64]map[string]any
	tools           map[int64]map[int64]map[string]any
	usage           map[string]any
	finished        bool
	done            bool
	hasOutput       bool
	expectedChoices int64
}

func newCompletion() *completion {
	return &completion{root: map[string]any{}, choices: map[int64]map[string]any{}, tools: map[int64]map[int64]map[string]any{}, expectedChoices: 1}
}

// Store pointers so a live Builder is never copied. String snapshots remain
// valid because these buffers only append; final JSON owns its encoded bytes.
type completionStringBuffer struct {
	text strings.Builder
}

func (b *completionStringBuffer) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.text.String())
}

func completionString(value any) string {
	if buffer, ok := value.(*completionStringBuffer); ok {
		return buffer.text.String()
	}
	return str(value)
}

func appendCompletionString(dst map[string]any, key, fragment string) {
	buffer, ok := dst[key].(*completionStringBuffer)
	if !ok {
		buffer = &completionStringBuffer{}
		buffer.text.WriteString(str(dst[key]))
		dst[key] = buffer
	}
	buffer.text.WriteString(fragment)
}

func (c *completion) observe(j map[string]any) {
	metadata := map[string]any{}
	for k, v := range j {
		switch k {
		case "choices", "usage", "object", "model", "obfuscation":
			// Aggregated separately, rewritten for Chat Completions, or
			// meaningful only for the original stream chunk.
		default:
			metadata[k] = v
		}
	}
	mergeObjects(c.root, metadata, false)
	if u := object(j["usage"]); u != nil {
		if c.usage == nil {
			c.usage = map[string]any{}
		}
		mergeObjects(c.usage, u, false)
	}
	for _, v := range list(j["choices"]) {
		ch := object(v)
		idx := number(ch["index"])
		dest := c.choices[idx]
		if dest == nil {
			dest = map[string]any{"index": idx, "message": map[string]any{"role": "assistant"}, "finish_reason": nil}
			c.choices[idx] = dest
		}
		delta := object(ch["delta"])
		msg := object(dest["message"])
		for k, v := range delta {
			switch k {
			case "tool_calls":
				if c.tools[idx] == nil {
					c.tools[idx] = map[int64]map[string]any{}
				}
				for _, tv := range list(v) {
					tc := object(tv)
					ti := number(tc["index"])
					t := c.tools[idx][ti]
					if t == nil {
						t = map[string]any{}
						c.tools[idx][ti] = t
					}
					for tk, tv := range tc {
						if tk == "index" {
							continue
						}
						if tk == "function" {
							f := object(t[tk])
							if f == nil {
								f = map[string]any{}
								t[tk] = f
							}
							mergeObjects(f, object(tv), true)
						} else if tv != nil {
							t[tk] = tv
						}
					}
				}
				c.hasOutput = true
			case "content", "reasoning_content", "reasoning", "refusal":
				if s := str(v); s != "" {
					appendCompletionString(msg, k, s)
					c.hasOutput = true
				}
			case "function_call":
				f := object(msg[k])
				if f == nil {
					f = map[string]any{}
					msg[k] = f
				}
				mergeObjects(f, object(v), true)
				c.hasOutput = true
			default:
				if v != nil {
					if m := object(v); m != nil {
						old := object(msg[k])
						if old == nil {
							old = map[string]any{}
							msg[k] = old
						}
						mergeObjects(old, m, false)
					} else if a, ok := v.([]any); ok {
						msg[k] = append(list(msg[k]), a...)
					} else {
						msg[k] = v
					}
				}
			}
		}
		if ch["finish_reason"] != nil {
			dest["finish_reason"] = ch["finish_reason"]
			c.finished = true
		}
		if ch["logprobs"] != nil {
			lp := object(dest["logprobs"])
			if lp == nil {
				lp = map[string]any{}
				dest["logprobs"] = lp
			}
			for key, value := range object(ch["logprobs"]) {
				if a, ok := value.([]any); ok {
					lp[key] = append(list(lp[key]), a...)
				} else if value != nil {
					lp[key] = value
				}
			}
		}
	}
}
func (c *completion) allFinished() bool {
	if len(c.choices) == 0 {
		return false
	}
	for _, ch := range c.choices {
		if strings.TrimSpace(str(ch["finish_reason"])) == "" {
			return false
		}
	}
	return true
}

// Some providers close a complete Chat stream without the terminal
// marker. Only accept a clean EOF when every choice is finished and any tool
// arguments are complete. Transport errors and partial SSE frames never use
// this fallback, and usage chunks arriving after finish_reason are still read.
func (c *completion) completeAtEOF() bool {
	if !c.hasOutput || !c.allFinished() {
		return false
	}
	if int64(len(c.choices)) != c.expectedChoices {
		return false
	}
	for i := int64(0); i < c.expectedChoices; i++ {
		if c.choices[i] == nil {
			return false
		}
	}
	validFunction := func(f map[string]any) bool {
		if strings.TrimSpace(completionString(f["name"])) == "" {
			return false
		}
		var args map[string]any
		return json.Unmarshal([]byte(completionString(f["arguments"])), &args) == nil && args != nil
	}
	for index, choice := range c.choices {
		tools := c.tools[index]
		legacy := object(object(choice["message"])["function_call"])
		if str(choice["finish_reason"]) == "tool_calls" && len(tools) == 0 {
			return false
		}
		if str(choice["finish_reason"]) == "function_call" && legacy == nil {
			return false
		}
		for _, tool := range tools {
			if strings.TrimSpace(str(tool["id"])) == "" || !validFunction(object(tool["function"])) {
				return false
			}
		}
		if legacy != nil && !validFunction(legacy) {
			return false
		}
	}
	return true
}

func (c *completion) result(model string) ([]byte, error) {
	if !c.done || !c.allFinished() {
		return nil, fail(502, "upstream stream ended before a completion and [DONE]")
	}
	if !c.hasOutput {
		return nil, fail(500, "empty response content")
	}
	out := c.root
	out["object"] = "chat.completion"
	out["model"] = model
	indices := []int{}
	for i := range c.choices {
		indices = append(indices, int(i))
	}
	sort.Ints(indices)
	choices := []any{}
	for _, i := range indices {
		ch := c.choices[int64(i)]
		if tools := c.tools[int64(i)]; len(tools) > 0 {
			ti := []int{}
			for k := range tools {
				ti = append(ti, int(k))
			}
			sort.Ints(ti)
			a := []any{}
			for _, k := range ti {
				a = append(a, tools[int64(k)])
			}
			object(ch["message"])["tool_calls"] = a
		}
		choices = append(choices, ch)
	}
	out["choices"] = choices
	if c.usage != nil {
		out["usage"] = c.usage
	}
	return json.Marshal(out)
}
func list(v any) []any { a, _ := v.([]any); return a }
func mergeObjects(dst, src map[string]any, concat bool) {
	for k, v := range src {
		if m := object(v); m != nil {
			d := object(dst[k])
			if d == nil {
				d = map[string]any{}
				dst[k] = d
			}
			mergeObjects(d, m, concat)
		} else if concat {
			if s, ok := v.(string); ok {
				appendCompletionString(dst, k, s)
			} else if v != nil {
				dst[k] = v
			}
		} else if v != nil {
			dst[k] = v
		}
	}
}
func unwrap(body []byte) (map[string]any, error) {
	j, e := decodeObject(body)
	if e != nil {
		return nil, upstreamProtocolFailure("upstream returned invalid JSON")
	}
	if j["error"] != nil || j["success"] == false {
		return nil, fail(500, errorMessage(j))
	}
	if j["success"] == true {
		d := object(j["data"])
		if d == nil {
			return nil, upstreamProtocolFailure("Cline success response has no data object")
		}
		j = d
	}
	if len(list(j["choices"])) == 0 {
		return nil, upstreamProtocolFailure("Cline response has no choices")
	}
	return j, nil
}

// degradedModelPattern matches the model name Cline reports for a request it
// served from a degraded pool rather than the one asked for: a suffix such as
// "vmc/deepseek-v4p1-flash-1017-contributor-fallbacks". The pool is a routing
// decision on the model tier, not on the provider: the same request can report
// finalProvider "deepseek" and still be served here, so this name is the only
// reliable signal that a request was degraded (2026-10-29, DeepSeek 1017).
var degradedModelPattern = regexp.MustCompile(`(?i)contributor-fallbacks?$|/fallbacks?$|-fallbacks?$`)

// observeServedModel records the model the upstream actually served and
// whether that was a degraded pool. The response's own "model" field is the
// only place Cline reports it, and the executor overwrites that field with the
// client's model name before anything downstream sees it, so it has to be read
// off each raw frame here.
func observeServedModel(j map[string]any, entry *LogEntry, attempt *Attempt) {
	served := strings.TrimSpace(str(j["model"]))
	if served == "" {
		return
	}
	entry.ServedModel = served
	attempt.ServedModel = served
	degraded := degradedModelPattern.MatchString(served)
	entry.Degraded = degraded
	attempt.Degraded = degraded
}

// Only explicit response routing facts count as actual providers. Candidate lists never do.
func observeMetadata(j map[string]any, entry *LogEntry, attempt *Attempt) {
	observeServerToolCalls(j, entry, attempt)
	observeServedModel(j, entry, attempt)
	apply := func(p, path string) {
		if p != "" {
			if strings.HasSuffix(attempt.ProviderSource, ".finalProvider") && !strings.HasSuffix(path, ".finalProvider") {
				return
			}
			if strings.HasSuffix(attempt.ProviderSource, ".resolvedProvider") && path == "provider" {
				return
			}
			entry.Provider = p
			entry.ProviderSource = path
			attempt.Provider = p
			attempt.ProviderSource = path
		}
	}
	if p := str(j["provider"]); p != "" {
		apply(p, "provider")
	}
	roots := []struct {
		v    map[string]any
		path string
	}{{j, ""}}
	for i, ch := range list(j["choices"]) {
		m := object(ch)
		for _, k := range []string{"delta", "message"} {
			if v := object(m[k]); v != nil {
				roots = append(roots, struct {
					v    map[string]any
					path string
				}{v, fmt.Sprintf("choices[%d].%s.", i, k)})
			}
		}
	}
	for _, root := range roots {
		p, path := responseRoutingProvider(root.v)
		apply(p, root.path+path)
	}
	if u := object(j["usage"]); u != nil {
		// Some SSE frames contain only part of usage. Missing fields must not
		// erase token counts already reported in a previous frame.
		if v, ok := u["prompt_tokens"]; ok && v != nil {
			entry.PromptTokens = number(v)
			entry.promptReported = true
			if entry.PromptUsageReported == nil {
				entry.PromptUsageReported = new(bool)
			}
			*entry.PromptUsageReported = true
		}
		if v, ok := u["completion_tokens"]; ok && v != nil {
			entry.CompletionTokens = number(v)
			entry.completionReported = true
			if entry.CompletionUsageReported == nil {
				entry.CompletionUsageReported = new(bool)
			}
			*entry.CompletionUsageReported = true
		}
		if entry.promptReported && entry.completionReported {
			entry.UsageReported = true
		}
		if d := object(u["prompt_tokens_details"]); d != nil {
			if v, ok := d["cached_tokens"]; ok && v != nil {
				entry.CachedTokens = number(v)
			}
			if v, ok := d["cache_write_tokens"]; ok && v != nil {
				entry.CacheWriteTokens, entry.CacheWriteReported = number(v), true
			}
		}
		if v, ok := u["cache_creation_input_tokens"]; ok && v != nil {
			entry.CacheWriteTokens, entry.CacheWriteReported = number(v), true
		}
		if v, ok := object(u["completion_tokens_details"])["reasoning_tokens"]; ok && v != nil {
			entry.ReasoningTokens = number(v)
		}
	}
}
func contentStarted(j map[string]any) bool {
	for _, ch := range list(j["choices"]) {
		d := object(object(ch)["delta"])
		if str(d["content"]) != "" || str(d["reasoning_content"]) != "" || str(d["reasoning"]) != "" || str(d["refusal"]) != "" || effectiveFunctionDelta(object(d["function_call"])) {
			return true
		}
		for _, tool := range list(d["tool_calls"]) {
			if effectiveFunctionDelta(object(object(tool)["function"])) {
				return true
			}
		}
	}
	return false
}

func effectiveFunctionDelta(function map[string]any) bool {
	return str(function["name"]) != "" || str(function["arguments"]) != ""
}
func isEmptyError(err error) bool {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		// Only the native provider's empty-response error may opt into the
		// configured one-time SSE fallback. Transport/limit/auth faults must not.
		if upstream.Kind != upstreamServiceKind && upstream.Kind != "upstream_error" {
			return false
		}
		if upstream.Status < 500 || (upstream.Scope != "" && upstream.Scope != "request") {
			return false
		}
	}
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "empty response content")
}

var errStreamDone = errors.New("stream complete")
