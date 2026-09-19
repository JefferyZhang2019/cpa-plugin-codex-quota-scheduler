package main

import (
	"testing"
)

func TestClassifyFrameResponsesHandshakeAndContent(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  frameDisposition
	}{
		{"created", `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}`, frameHandshake},
		{"in progress", `{"type":"response.in_progress"}`, frameHandshake},
		{"rate limits", `{"type":"codex.rate_limits","plan_type":"plus"}`, frameHandshake},
		{"keepalive event", `event: keepalive
data: {"type":"keepalive","sequence_number":1}`, frameHandshake},
		{"sse comment keepalive", `: keepalive`, frameHandshake},
		{"sse comment empty", `:`, frameHandshake},
		{"empty item announcement", `{"type":"response.output_item.added","item":{"type":"message","content":[]}}`, frameHandshake},
		{"reasoning announcement", `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`, frameHandshake},
		{"function call announcement", `{"type":"response.output_item.added","item":{"type":"function_call","arguments":""}}`, frameHandshake},
		{"empty part", `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`, frameHandshake},
		{"text fragment", `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hello"}`, frameContent},
		{"item with content", `{"type":"response.output_item.added","item":{"type":"message","content":[{"type":"output_text","text":"hi"}]}}`, frameContent},
		{"encrypted reasoning item", `{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"abc"}}`, frameContent},
		{"reasoning summary delta", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, frameContent},
		{"completed", `{"type":"response.completed","response":{"id":"resp_1"}}`, frameTerminal},
		{"incomplete", `{"type":"response.incomplete"}`, frameTerminal},
		{"done sentinel", `[DONE]`, frameTerminal},
		{"unknown json frame", `{"type":"something.new"}`, frameContent},
		{"unrecognized non json frame", `plain text payload`, frameContent},
	}
	for _, testCase := range cases {
		verdict := classifyStreamPayload([]byte(testCase.frame), protocolCodex)
		if verdict.Disposition != testCase.want {
			t.Fatalf("%s: disposition = %v, want %v", testCase.name, verdict.Disposition, testCase.want)
		}
	}
}

func TestClassifyFrameTreatsCapacityFrameAsFailure(t *testing.T) {
	frame := `{"type":"error","error":{"message":"Selected model is at capacity. Please try a different model."},"sequence_number":2}`
	verdict := classifyStreamPayload([]byte(frame), protocolCodex)
	if verdict.Disposition != frameFailure {
		t.Fatalf("disposition = %v, want failure", verdict.Disposition)
	}
	if !verdict.Failure.Retryable || verdict.Failure.Kind != retryKindCapacity {
		t.Fatalf("classification = %+v", verdict.Failure)
	}
}

func TestClassifyFrameTreatsResponseFailedAsFailure(t *testing.T) {
	frame := `event: response.failed
data: {"type":"response.failed","response":{"error":{"type":"service_unavailable_error","message":"overloaded"}}}`
	verdict := classifyStreamPayload([]byte(frame), protocolCodex)
	if verdict.Disposition != frameFailure || !verdict.Failure.Retryable {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.Failure.Kind != retryKindOverload {
		t.Fatalf("kind = %q", verdict.Failure.Kind)
	}
}

func TestClassifyFrameClaudeProtocol(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  frameDisposition
	}{
		{"message start", `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}`, frameHandshake},
		{"ping", `event: ping
data: {"type":"ping"}`, frameHandshake},
		{"content block start", `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, frameHandshake},
		{"content block delta", `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, frameContent},
		{"message stop", `event: message_stop
data: {"type":"message_stop"}`, frameTerminal},
		{"overloaded error", `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, frameFailure},
	}
	for _, testCase := range cases {
		verdict := classifyStreamPayload([]byte(testCase.frame), protocolClaude)
		if verdict.Disposition != testCase.want {
			t.Fatalf("%s: disposition = %v, want %v", testCase.name, verdict.Disposition, testCase.want)
		}
	}
}

func TestClassifyFrameOpenAIProtocol(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  frameDisposition
	}{
		{"role only delta", `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`, frameHandshake},
		{"empty content delta", `data: {"id":"1","choices":[{"index":0,"delta":{"content":""}}]}`, frameHandshake},
		{"content delta", `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hello"}}]}`, frameContent},
		{"tool call delta", `data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`, frameContent},
		{"finish reason", `data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, frameContent},
		{"error object", `data: {"error":{"message":"model is at capacity","type":"capacity"}}`, frameFailure},
		{"usage only", `data: {"id":"1","usage":{"prompt_tokens":1}}`, frameContent},
	}
	for _, testCase := range cases {
		verdict := classifyStreamPayload([]byte(testCase.frame), protocolOpenAI)
		if verdict.Disposition != testCase.want {
			t.Fatalf("%s: disposition = %v, want %v", testCase.name, verdict.Disposition, testCase.want)
		}
	}
}

func TestClassifyStreamPayloadScansMultipleFramesInOrder(t *testing.T) {
	payload := `event: response.created
data: {"type":"response.created"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

`
	verdict := classifyStreamPayload([]byte(payload), protocolCodex)
	if verdict.Disposition != frameContent {
		t.Fatalf("disposition = %v, want content", verdict.Disposition)
	}
	payload = `event: response.created
data: {"type":"response.created"}

event: error
data: {"type":"error","error":{"message":"Selected model is at capacity."}}

`
	verdict = classifyStreamPayload([]byte(payload), protocolCodex)
	if verdict.Disposition != frameFailure {
		t.Fatalf("disposition = %v, want failure from the trailing error frame", verdict.Disposition)
	}
	payload = `event: response.created
data: {"type":"response.created"}

`
	if verdict = classifyStreamPayload([]byte(payload), protocolCodex); verdict.Disposition != frameHandshake {
		t.Fatalf("handshake-only payload = %v", verdict.Disposition)
	}
}

func TestClassifyStreamPayloadHoldsFragmentsButCommitsUnknownJSON(t *testing.T) {
	fragment := `event: response.output_text.delta
data: {"type":"response.output_text.delta","del`
	if verdict := classifyStreamPayload([]byte(fragment), protocolCodex); verdict.Disposition != frameHandshake {
		t.Fatalf("split frame = %v, want handshake while it is still arriving", verdict.Disposition)
	}
	partialJSON := `{"type":"response.output_ite`
	if verdict := classifyStreamPayload([]byte(partialJSON), protocolCodex); verdict.Disposition != frameHandshake {
		t.Fatalf("partial json body = %v, want handshake", verdict.Disposition)
	}
	unknown := `{"unexpected":"shape"}`
	if verdict := classifyStreamPayload([]byte(unknown), protocolCodex); verdict.Disposition != frameContent {
		t.Fatalf("unknown json frame = %v, want content so it is never replayed", verdict.Disposition)
	}
}

func TestRetryFrameAssemblerBuffersSplitFrames(t *testing.T) {
	assembler := retryFrameAssembler{protocol: protocolCodex}
	head := []byte(`event: response.output_text.delta
data: {"type":"response.output_text.del`)
	if _, decisive := assembler.absorb(head); decisive {
		t.Fatal("a split frame must not decide the attempt")
	}
	tail := []byte(`ta","delta":"hello"}

`)
	verdict, decisive := assembler.absorb(tail)
	if !decisive || verdict.Disposition != frameContent {
		t.Fatalf("reassembled frame = %+v/%v", verdict, decisive)
	}
}

func TestRetryFrameAssemblerHandlesBareJSONBodies(t *testing.T) {
	assembler := retryFrameAssembler{protocol: protocolOpenAI}
	if _, decisive := assembler.absorb([]byte(`{"type":"resp`)); decisive {
		t.Fatal("a partial JSON body must not decide the attempt")
	}
	if _, decisive := assembler.absorb([]byte(`onse.created"}`)); decisive {
		t.Fatal("a handshake body must not decide the attempt")
	}
	verdict, decisive := assembler.absorb([]byte(`{"type":"error","error":{"message":"model is at capacity"}}`))
	if !decisive || verdict.Disposition != frameFailure {
		t.Fatalf("bare capacity body = %+v/%v", verdict, decisive)
	}
}

func TestRetryFrameAssemblerIgnoresHeartbeatsBetweenFrames(t *testing.T) {
	assembler := retryFrameAssembler{protocol: protocolCodex}
	for _, frame := range []string{": keepalive\n\n", "event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n"} {
		if _, decisive := assembler.absorb([]byte(frame)); decisive {
			t.Fatalf("heartbeat %q decided the attempt", frame)
		}
	}
	verdict, decisive := assembler.absorb([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"model is at capacity\"}}\n\n"))
	if !decisive || verdict.Disposition != frameFailure {
		t.Fatalf("capacity frame after heartbeats = %+v/%v", verdict, decisive)
	}
}

func TestRetryHoldBufferTracksBudgets(t *testing.T) {
	buffer := retryHoldBuffer{limitFrames: 2, limitBytes: 1024}
	buffer.add([]byte("a\n\n"))
	if buffer.overBudget() {
		t.Fatal("one frame must fit a two-frame budget")
	}
	buffer.add([]byte("b\n\n"))
	if buffer.overBudget() {
		t.Fatal("two frames must fit a two-frame budget")
	}
	buffer.add([]byte("c\n\n"))
	if !buffer.overBudget() {
		t.Fatal("three frames must exceed a two-frame budget")
	}
	bytesBudget := retryHoldBuffer{limitFrames: 4096, limitBytes: 8}
	bytesBudget.add([]byte("123456789"))
	if !bytesBudget.overBudget() {
		t.Fatal("nine bytes must exceed an eight-byte budget")
	}
	unlimited := retryHoldBuffer{}
	unlimited.add([]byte("anything at all"))
	if unlimited.overBudget() {
		t.Fatal("a buffer without limits must never report a budget breach")
	}
}
