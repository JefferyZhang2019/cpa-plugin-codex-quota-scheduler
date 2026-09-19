package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// frameDisposition is what a stream frame means for the retry decision.
//
// handshake - nothing observable has happened downstream yet, holding is safe.
// content   - observable output exists, the attempt is committed and must not retry.
// terminal  - the attempt finished; buffered frames get flushed.
// failure   - the attempt failed in a way the classifier judges.
type frameDisposition int

const (
	frameHandshake frameDisposition = iota
	frameContent
	frameTerminal
	frameFailure
)

// frameVerdict pairs a disposition with the classification of a failure frame.
type frameVerdict struct {
	Disposition frameDisposition
	Failure     RetryClassification
}

// classifyStreamPayload inspects one raw stream payload and reports the first
// decisive frame it contains. Frames are scanned in order and handshake frames
// are skipped, so a payload holding "response.created" plus an error frame is
// judged by the error, while a payload holding a text delta is judged committed.
//
// An incomplete trailing frame is ignored here; the executor keeps it buffered
// until its terminator arrives, so this never acts on partial JSON.
func classifyStreamPayload(payload []byte, protocol string) frameVerdict {
	return classifyStreamBuffer(payload, protocol, false)
}

// classifyStreamBuffer scans complete SSE frames. When completeOnly is false a
// trailing fragment without its terminator is passed to the frame classifier so
// single-frame non-SSE payloads are still judged.
func classifyStreamBuffer(buffer []byte, protocol string, completeOnly bool) frameVerdict {
	trimmed := bytes.TrimSpace(buffer)
	if len(trimmed) == 0 {
		return frameVerdict{Disposition: frameHandshake}
	}
	remaining := buffer
	sawFrame := false
	for {
		index := bytes.Index(remaining, []byte("\n\n"))
		if index < 0 {
			if !completeOnly && !sawFrame && len(bytes.TrimSpace(remaining)) > 0 {
				return classifyFrameFragment(remaining, protocol)
			}
			if !sawFrame {
				return classifyFrameFragment(remaining, protocol)
			}
			return frameVerdict{Disposition: frameHandshake}
		}
		frame := remaining[:index]
		remaining = remaining[index+2:]
		sawFrame = true
		verdict := classifyFrame(frame, protocol)
		if verdict.Disposition != frameHandshake {
			return verdict
		}
		if len(bytes.TrimSpace(remaining)) == 0 {
			return frameVerdict{Disposition: frameHandshake}
		}
	}
}

// classifyFrame classifies a single SSE frame or a bare JSON payload.
func classifyFrame(frame []byte, protocol string) frameVerdict {
	eventName, data, found := splitSSEFrame(frame)
	if !found {
		if isSSEControlFrame(frame) {
			// A comment-only heartbeat or another control line carries nothing a
			// client can observe, so the attempt is still holdable.
			return frameVerdict{Disposition: frameHandshake}
		}
		data = frame
	}
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) == 0 {
		// SSE heartbeat or comment-only frame: nothing observable happened.
		return frameVerdict{Disposition: frameHandshake}
	}
	if bytes.Equal(trimmedData, []byte("[DONE]")) {
		return frameVerdict{Disposition: frameTerminal}
	}
	if !json.Valid(trimmedData) {
		// Not a shape this list understands. Treat it as committed output so an
		// unrecognized frame can never be replayed against another target.
		return frameVerdict{Disposition: frameContent}
	}
	// The Responses event namespace is unambiguous, and some upstreams deliver
	// those events as bare JSON objects with no SSE framing. Judging them by
	// their own classifier keeps a "response.created" body from being read as
	// unknown content just because the endpoint negotiated another protocol.
	if isResponsesEventBody(trimmedData) {
		return classifyResponsesFrame(trimmedData)
	}
	switch protocol {
	case protocolClaude:
		return classifyClaudeFrame(eventName, trimmedData)
	case protocolOpenAI:
		return classifyChatCompletionFrame(trimmedData)
	default:
		return classifyResponsesFrame(trimmedData)
	}
}

// isResponsesEventBody reports whether a JSON body carries a Responses or Codex
// event type.
func isResponsesEventBody(data []byte) bool {
	eventType := strings.TrimSpace(jsonStringField(data, "type"))
	if eventType == "" {
		return false
	}
	return strings.HasPrefix(eventType, "response.") || strings.HasPrefix(eventType, "codex.")
}

// classifyFrameFragment judges a payload that arrived without a frame
// terminator. Such a payload may be a prefix of a frame that is still on the
// wire, so anything that does not parse as JSON is held instead of committing
// the attempt: holding costs a later retry, while committing on a fragment would
// disable it for good.
func classifyFrameFragment(frame []byte, protocol string) frameVerdict {
	verdict := classifyFrame(frame, protocol)
	if verdict.Disposition != frameContent {
		return verdict
	}
	// The frame is unrecognized. That only commits the attempt when the payload
	// is actually complete: a JSON body, or an SSE frame whose data line parses.
	// A truncated frame is held so a slow upstream keeps its retry.
	trimmed := bytes.TrimSpace(frame)
	if json.Valid(trimmed) {
		return verdict
	}
	if _, data, found := splitSSEFrame(frame); found && json.Valid(bytes.TrimSpace(data)) {
		return verdict
	}
	if !looksLikeIncompleteFrame(frame) {
		return verdict
	}
	return frameVerdict{Disposition: frameHandshake}
}

// looksLikeIncompleteFrame reports whether a payload could be the prefix of an
// SSE frame or a JSON body that is still arriving.
func looksLikeIncompleteFrame(frame []byte) bool {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return true
	}
	switch trimmed[0] {
	case '{', '[', ':':
		return true
	}
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", "comment:"} {
		if bytes.HasPrefix(trimmed, []byte(prefix)) {
			return true
		}
	}
	return false
}

// isSSEControlFrame reports whether every non-empty line of a frame is an SSE
// comment or a field that never carries output.
func isSSEControlFrame(frame []byte) bool {
	sawLine := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if len(line) == 0 {
			continue
		}
		sawLine = true
		if bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte("retry:")) ||
			bytes.HasPrefix(line, []byte("comment:")) {
			continue
		}
		return false
	}
	return sawLine
}

// countSSEFrames counts the frames a payload holds: one per blank-line
// terminator, plus one for a trailing fragment that has not been terminated yet.
func countSSEFrames(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	count := bytes.Count(payload, []byte("\n\n"))
	tail := payload
	for {
		index := bytes.Index(tail, []byte("\n\n"))
		if index < 0 {
			break
		}
		tail = tail[index+2:]
	}
	if len(bytes.TrimSpace(tail)) > 0 {
		count++
	}
	return count
}

// splitSSEFrame extracts the event name and data payload from one frame.
func splitSSEFrame(frame []byte) (string, []byte, bool) {
	event := ""
	var data bytes.Buffer
	found := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimSpace(line[len("data:"):]))
			found = true
		}
	}
	return event, data.Bytes(), found
}

func classifyResponsesFrame(data []byte) frameVerdict {
	if failure, ok := responsesFailure(data); ok {
		return frameVerdict{Disposition: frameFailure, Failure: failure}
	}
	eventType := strings.TrimSpace(jsonStringField(data, "type"))
	switch eventType {
	case "response.created", "response.in_progress", "response.queued", "keepalive", "codex.rate_limits":
		return frameVerdict{Disposition: frameHandshake}
	case "response.output_item.added":
		if isBufferableOutputItem(data) {
			return frameVerdict{Disposition: frameHandshake}
		}
		return frameVerdict{Disposition: frameContent}
	case "response.content_part.added", "response.reasoning_summary_part.added":
		if isBufferablePart(data) {
			return frameVerdict{Disposition: frameHandshake}
		}
		return frameVerdict{Disposition: frameContent}
	case "response.completed", "response.incomplete":
		return frameVerdict{Disposition: frameTerminal}
	default:
		return frameVerdict{Disposition: frameContent}
	}
}

// responsesFailure recognizes the two shapes OpenAI-compatible upstreams use to
// report an in-band failure after an HTTP 200: a bare error frame, and a
// response.failed event carrying response.error.
func responsesFailure(data []byte) (RetryClassification, bool) {
	if strings.EqualFold(strings.TrimSpace(jsonStringField(data, "type")), "response.failed") {
		return classifyRetryFailure(string(data)), true
	}
	if len(jsonField(data, "error")) > 0 {
		return classifyRetryFailure(string(data)), true
	}
	return RetryClassification{}, false
}

func classifyClaudeFrame(eventName string, data []byte) frameVerdict {
	eventType := strings.TrimSpace(jsonStringField(data, "type"))
	if eventType == "" {
		eventType = eventName
	}
	switch eventType {
	case "error":
		return frameVerdict{Disposition: frameFailure, Failure: classifyRetryFailure(string(data))}
	case "ping", "message_start", "content_block_start", "content_block_stop", "message_delta":
		if len(jsonField(data, "error")) > 0 {
			return frameVerdict{Disposition: frameFailure, Failure: classifyRetryFailure(string(data))}
		}
		return frameVerdict{Disposition: frameHandshake}
	case "message_stop":
		return frameVerdict{Disposition: frameTerminal}
	default:
		return frameVerdict{Disposition: frameContent}
	}
}

func classifyChatCompletionFrame(data []byte) frameVerdict {
	if len(jsonField(data, "error")) > 0 {
		return frameVerdict{Disposition: frameFailure, Failure: classifyRetryFailure(string(data))}
	}
	if choices := jsonField(data, "choices"); len(choices) > 0 {
		if hasChatCompletionContent(choices) {
			return frameVerdict{Disposition: frameContent}
		}
		return frameVerdict{Disposition: frameHandshake}
	}
	return frameVerdict{Disposition: frameContent}
}

// hasChatCompletionContent reports whether a choices array already carries text,
// tool arguments, or a finish reason. A role-only delta is a handshake.
func hasChatCompletionContent(choices []byte) bool {
	var parsed []map[string]any
	if err := json.Unmarshal(choices, &parsed); err != nil {
		return true
	}
	for _, choice := range parsed {
		if finish, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finish) != "" {
			return true
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			message, _ = choice["delta"].(map[string]any)
		}
		if message == nil {
			continue
		}
		if content, ok := message["content"].(string); ok && content != "" {
			return true
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			return true
		}
		if calls, ok := message["function_call"].(map[string]any); ok && len(calls) > 0 {
			return true
		}
		if refusal, ok := message["refusal"].(string); ok && refusal != "" {
			return true
		}
	}
	return false
}

// isBufferableOutputItem mirrors CPA's isCodexBufferableOutputItem: only an
// announced item that has not started producing is safe to hold back.
func isBufferableOutputItem(data []byte) bool {
	item := jsonField(data, "item")
	if len(item) == 0 {
		return false
	}
	switch strings.TrimSpace(jsonStringField(item, "type")) {
	case "message":
		return isEmptyContentList(jsonField(item, "content"))
	case "reasoning":
		if strings.TrimSpace(jsonStringField(item, "encrypted_content")) != "" {
			return false
		}
		return isEmptyContentList(jsonField(item, "summary")) && isEmptyContentList(jsonField(item, "content"))
	case "function_call":
		return strings.TrimSpace(jsonStringField(item, "arguments")) == ""
	case "custom_tool_call":
		return strings.TrimSpace(jsonStringField(item, "input")) == ""
	default:
		return false
	}
}

func isBufferablePart(data []byte) bool {
	part := jsonField(data, "part")
	if len(part) == 0 {
		return false
	}
	switch strings.TrimSpace(jsonStringField(part, "type")) {
	case "output_text", "summary_text", "text", "reasoning_text":
		return strings.TrimSpace(jsonStringField(part, "text")) == ""
	case "refusal":
		return strings.TrimSpace(jsonStringField(part, "refusal")) == ""
	default:
		return false
	}
}

// isEmptyContentList reports whether every entry is a textual shape that is
// still empty. An unknown entry type may carry content this check cannot see, so
// it is treated as already produced.
func isEmptyContentList(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false
	}
	for _, entry := range entries {
		entryType, _ := entry["type"].(string)
		switch entryType {
		case "output_text", "summary_text", "text", "reasoning_text":
			if text, _ := entry["text"].(string); text != "" {
				return false
			}
		case "refusal":
			if refusal, _ := entry["refusal"].(string); refusal != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// jsonField returns the raw JSON value of a top-level object field.
func jsonField(data []byte, field string) []byte {
	if len(data) == 0 || field == "" {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil
	}
	return object[field]
}
