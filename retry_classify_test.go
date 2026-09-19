package main

import (
	"strings"
	"testing"
)

func TestClassifyRetryFailureRecognizesCapacityFrames(t *testing.T) {
	cases := []struct {
		name string
		text string
		kind retryFailureKind
	}{
		{
			name: "live codex capacity frame",
			text: `{"type":"error","error":{"message":"Selected model is at capacity. Please try a different model."},"sequence_number":2}`,
			kind: retryKindCapacity,
		},
		{
			name: "capacity code",
			text: `{"type":"error","error":{"code":"model_at_capacity","message":"busy"}}`,
			kind: retryKindCapacity,
		},
		{
			name: "capacity type",
			text: `{"error":{"type":"model_is_at_capacity"}}`,
			kind: retryKindCapacity,
		},
		{
			name: "bare capacity wording",
			text: "upstream said the model is at capacity right now",
			kind: retryKindCapacity,
		},
		{
			name: "service unavailable",
			text: `{"type":"error","error":{"type":"service_unavailable_error","message":"try later"}}`,
			kind: retryKindOverload,
		},
		{
			name: "server is overloaded",
			text: `{"error":{"code":"server_is_overloaded"}}`,
			kind: retryKindOverload,
		},
		{
			name: "rate limit type",
			text: `{"error":{"type":"rate_limit_error"}}`,
			kind: retryKindRateLimit,
		},
		{
			name: "rate limit code",
			text: `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`,
			kind: retryKindRateLimit,
		},
		{
			name: "server error with retry hint",
			text: `{"error":{"type":"server_error","message":"Something went wrong. You can retry your request."}}`,
			kind: retryKindOverload,
		},
		{
			name: "claude overloaded",
			text: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			kind: retryKindOverload,
		},
		{
			name: "gemini resource exhausted",
			text: `{"error":{"code":"resource_exhausted","message":"quota"}}`,
			kind: retryKindOverload,
		},
		{
			name: "prose wrapping the body",
			text: `upstream request failed: {"error":{"type":"service_unavailable_error"}} (see logs)`,
			kind: retryKindOverload,
		},
	}
	for _, testCase := range cases {
		verdict := classifyRetryFailure(testCase.text)
		if !verdict.Retryable {
			t.Fatalf("%s: not retryable: %+v", testCase.name, verdict)
		}
		if verdict.Kind != testCase.kind {
			t.Fatalf("%s: kind = %q, want %q", testCase.name, verdict.Kind, testCase.kind)
		}
		if verdict.Reason == "" {
			t.Fatalf("%s: empty reason", testCase.name)
		}
	}
}

func TestClassifyRetryStatusMatchesBootstrapEligibility(t *testing.T) {
	retryable := map[int]retryFailureKind{
		429: retryKindRateLimit,
		500: retryKindOverload,
		502: retryKindOverload,
		503: retryKindOverload,
		504: retryKindOverload,
		529: retryKindOverload,
	}
	for status, kind := range retryable {
		verdict := classifyRetryStatus(status)
		if !verdict.Retryable {
			t.Fatalf("status %d: not retryable", status)
		}
		if verdict.Kind != kind {
			t.Fatalf("status %d: kind = %q, want %q", status, verdict.Kind, kind)
		}
		if verdict.Status != status {
			t.Fatalf("status %d: reported %d", status, verdict.Status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 422} {
		verdict := classifyRetryStatus(status)
		if verdict.Retryable {
			t.Fatalf("status %d must not be retryable: %+v", status, verdict)
		}
		if verdict.Status != status {
			t.Fatalf("status %d: reported %d", status, verdict.Status)
		}
	}
}

func TestClassifyRetryFailureRecognizesStatusAndTransportText(t *testing.T) {
	cases := []struct {
		text string
		kind retryFailureKind
	}{
		{"upstream returned status 503", retryKindStatus},
		{"502", retryKindStatus},
		{"429 too many requests", retryKindStatus},
		{"connection reset by peer", retryKindTransport},
		{"read tcp: i/o timeout", retryKindTransport},
		{"unexpected EOF", retryKindTransport},
		{"context deadline exceeded", retryKindTransport},
		{"dial tcp: no such host", retryKindTransport},
		{"upstream stream closed before the first chunk", retryKindTransport},
	}
	for _, testCase := range cases {
		verdict := classifyRetryFailure(testCase.text)
		if !verdict.Retryable || verdict.Kind != testCase.kind {
			t.Fatalf("%q: %+v", testCase.text, verdict)
		}
	}
}

func TestClassifyRetryFailureFailsClosedOnUnknownText(t *testing.T) {
	cases := []string{
		"",
		"   ",
		`{"error":{"type":"invalid_request_error","message":"unknown parameter temperature"}}`,
		`{"error":{"type":"authentication_error","message":"invalid api key"}}`,
		"model gpt-5.6-sol does not exist",
		"the request was rejected because it contains an unsupported field",
		`{"error":{"message":"context length 128000 exceeded"}}`,
	}
	for _, text := range cases {
		verdict := classifyRetryFailure(text)
		if verdict.Retryable {
			t.Fatalf("%q must not be retryable: %+v", text, verdict)
		}
	}
}

func TestClassifyRetryFailureDoesNotReadStatusFromUnrelatedNumbers(t *testing.T) {
	for _, text := range []string{
		"invalid_request_error: max_output_tokens 5012 exceeds the limit",
		"request id 15003 not found",
		`{"error":{"type":"invalid_request_error","message":"model gpt-5.6-sol-2026 requires 5003 tokens"}}`,
	} {
		if verdict := classifyRetryFailure(text); verdict.Retryable {
			t.Fatalf("%q must not be retryable: %+v", text, verdict)
		}
	}
}

func TestExtractJSONErrorObjectHandlesEmbeddedAndMalformedBodies(t *testing.T) {
	body := extractJSONErrorObject(`prefix {"a":{"b":"}"}} suffix`)
	if !strings.Contains(string(body), `"b":"}"`) {
		t.Fatalf("extractJSONErrorObject = %s", body)
	}
	if body := extractJSONErrorObject("no json here"); body != nil {
		t.Fatalf("extractJSONErrorObject returned %s for plain text", body)
	}
	if body := extractJSONErrorObject(`{"broken":`); body != nil {
		t.Fatalf("extractJSONErrorObject returned %s for a truncated object", body)
	}
	if got := jsonStringField([]byte(`{"error":{"code":503}}`), "error", "code"); got != "503" {
		t.Fatalf("jsonStringField numeric = %q, want 503", got)
	}
	if got := jsonStringField([]byte(`{"error":{"code":503.5}}`), "error", "code"); got != "" {
		t.Fatalf("jsonStringField fractional = %q, want empty", got)
	}
}

func TestClassifyAttemptErrorKeepsHostStatus(t *testing.T) {
	verdict := classifyAttemptError(&hostCallError{Code: "upstream_error", Message: "bad gateway", Status: 502})
	if !verdict.Retryable || verdict.Status != 502 || verdict.Kind != retryKindOverload {
		t.Fatalf("classifyAttemptError = %+v", verdict)
	}
	verdict = classifyAttemptError(&hostCallError{Status: 401, Message: "unauthorized"})
	if verdict.Retryable {
		t.Fatalf("401 must not be retryable: %+v", verdict)
	}
	verdict = classifyAttemptError(errFakeTransport)
	if !verdict.Retryable || verdict.Kind != retryKindTransport {
		t.Fatalf("transport error = %+v", verdict)
	}
}

var errFakeTransport = fakeTransportError{}

type fakeTransportError struct{}

func (fakeTransportError) Error() string { return "read: connection reset by peer" }

func TestRetryFailureTextKeepsUpstreamCopy(t *testing.T) {
	verdict := classifyRetryFailure(`{"error":{"message":"Selected model is at capacity."}}`)
	text := retryFailureText(`{"error":{"message":"Selected model is at capacity."}}`, verdict)
	if !strings.Contains(text, "at capacity") {
		t.Fatalf("retryFailureText dropped the upstream copy: %q", text)
	}
	overload := classifyRetryFailure(`{"error":{"type":"service_unavailable_error"}}`)
	text = retryFailureText(`{"error":{"type":"service_unavailable_error"}}`, overload)
	if !strings.Contains(text, "server overloaded") {
		t.Fatalf("retryFailureText did not annotate the reason: %q", text)
	}
	if got := retryFailureText("", RetryClassification{}); got != "upstream failure" {
		t.Fatalf("retryFailureText(empty) = %q", got)
	}
}
