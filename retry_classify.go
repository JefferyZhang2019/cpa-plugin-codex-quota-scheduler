package main

import (
	"encoding/json"
	"strings"
	"unicode"
)

// retryFailureKind labels why an attempt was considered retryable. The label is
// reported in the status page and in logs; only Retryable drives behavior.
type retryFailureKind string

const (
	retryKindCapacity  retryFailureKind = "capacity"
	retryKindOverload  retryFailureKind = "overload"
	retryKindRateLimit retryFailureKind = "rate_limit"
	retryKindStatus    retryFailureKind = "http_status"
	retryKindTransport retryFailureKind = "transport"
)

// RetryClassification is the verdict for one observed failure.
type RetryClassification struct {
	Retryable bool
	Kind      retryFailureKind
	Reason    string
	Status    int
}

// retryableStatuses are the upstream statuses the chain treats as transient.
// They mirror CPA's own bootstrap-retry eligibility set plus 529, which some
// upstreams use for overload.
var retryableStatuses = map[int]struct{}{
	429: {},
	500: {},
	502: {},
	503: {},
	504: {},
	529: {},
}

// retryTransportMarkers are substrings that indicate the failure happened
// before the upstream produced a usable response, so another attempt is safe.
var retryTransportMarkers = []string{
	"connection reset",
	"connection refused",
	"broken pipe",
	"unexpected eof",
	"i/o timeout",
	"deadline exceeded",
	"context deadline",
	"no such host",
	"tls handshake",
	"transport failure",
	"stream closed before",
	"upstream stream closed",
}

// classifyRetryFailure decides whether a failure observed before any content was
// committed justifies another attempt.
//
// The input is the flattened error text the host hands back for a failed nested
// execution or stream read. It may be a bare JSON error body, a JSON body with
// surrounding prose, or a transport message. Unknown text is deliberately not
// retryable: retrying on a shape this list has not been taught about risks
// repeating a request whose failure was not transient.
func classifyRetryFailure(text string) RetryClassification {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return RetryClassification{Reason: "empty failure text"}
	}

	body := extractJSONErrorObject(trimmed)
	// In-band stream failures arrive either as a bare error object or wrapped in
	// a "response.failed" event whose cause lives under response.error, so both
	// locations are consulted before falling back to the top level.
	errorType := firstNonEmpty(jsonStringField(body, "error", "type"), jsonStringField(body, "response", "error", "type"))
	errorCode := firstNonEmpty(jsonStringField(body, "error", "code"), jsonStringField(body, "response", "error", "code"))
	errorMessage := firstNonEmpty(jsonStringField(body, "error", "message"), jsonStringField(body, "response", "error", "message"))
	errorType = strings.ToLower(strings.TrimSpace(errorType))
	errorCode = strings.ToLower(strings.TrimSpace(errorCode))
	errorMessage = strings.ToLower(strings.TrimSpace(errorMessage))
	if errorMessage == "" {
		errorMessage = strings.ToLower(strings.TrimSpace(jsonStringField(body, "message")))
	}
	topType := strings.ToLower(strings.TrimSpace(jsonStringField(body, "type")))
	topCode := strings.ToLower(strings.TrimSpace(jsonStringField(body, "code")))
	lower := strings.ToLower(trimmed)

	if isModelCapacityText(errorMessage) || isModelCapacityText(lower) {
		return RetryClassification{Retryable: true, Kind: retryKindCapacity, Reason: "model at capacity"}
	}
	switch {
	case errorType == "service_unavailable_error", errorCode == "server_is_overloaded":
		return RetryClassification{Retryable: true, Kind: retryKindOverload, Reason: "server overloaded"}
	case errorType == "rate_limit_error", errorCode == "rate_limit_exceeded":
		return RetryClassification{Retryable: true, Kind: retryKindRateLimit, Reason: "rate limited"}
	case (errorType == "server_error" || errorCode == "server_error") && strings.Contains(errorMessage, "you can retry your request"):
		return RetryClassification{Retryable: true, Kind: retryKindOverload, Reason: "server error with retry hint"}
	case errorType == "overloaded_error", topType == "overloaded_error", errorCode == "overloaded_error":
		return RetryClassification{Retryable: true, Kind: retryKindOverload, Reason: "overloaded"}
	case errorCode == "resource_exhausted", topCode == "resource_exhausted":
		return RetryClassification{Retryable: true, Kind: retryKindOverload, Reason: "resource exhausted"}
	}
	if status, ok := retryableStatusInText(lower); ok {
		return RetryClassification{Retryable: true, Kind: retryKindStatus, Reason: "upstream status", Status: status}
	}
	if marker, ok := transportMarkerInText(lower); ok {
		return RetryClassification{Retryable: true, Kind: retryKindTransport, Reason: marker}
	}
	return RetryClassification{Reason: "unrecognized failure"}
}

// firstNonEmpty returns the first non-empty value, so a nested error shape can
// take precedence over a fallback without nesting conditionals.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// isModelCapacityText mirrors CPA's isCodexModelCapacityError so a chain reacts
// to the same strings the executor already treats as capacity rejections.
func isModelCapacityText(text string) bool {
	if text == "" {
		return false
	}
	if strings.Contains(text, "model is at capacity") ||
		strings.Contains(text, "model_at_capacity") ||
		strings.Contains(text, "model_is_at_capacity") ||
		strings.Contains(text, "at capacity") {
		return true
	}
	return strings.Contains(text, "model") && strings.Contains(text, "capacity")
}

// retryableStatusInText finds a retryable HTTP status used as a standalone
// number, so a status embedded in an identifier or a duration cannot match.
func retryableStatusInText(text string) (int, bool) {
	for _, token := range numericTokens(text) {
		status := 0
		for _, digit := range token {
			status = status*10 + int(digit-'0')
		}
		if _, ok := retryableStatuses[status]; ok {
			return status, true
		}
	}
	return 0, false
}

func numericTokens(text string) []string {
	tokens := make([]string, 0, 4)
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsDigit(r) {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

func transportMarkerInText(text string) (string, bool) {
	for _, marker := range retryTransportMarkers {
		if strings.Contains(text, marker) {
			return marker, true
		}
	}
	return "", false
}

// classifyRetryStatus turns a host-reported status code into a verdict. The
// chain retries the statuses CPA already treats as transient for bootstrap
// retries, plus 504 and 529 for upstreams that report overload that way.
func classifyRetryStatus(status int) RetryClassification {
	if _, ok := retryableStatuses[status]; ok {
		kind := retryKindStatus
		switch status {
		case 429:
			kind = retryKindRateLimit
		case 500, 502, 503, 504, 529:
			kind = retryKindOverload
		}
		return RetryClassification{
			Retryable: true,
			Kind:      kind,
			Reason:    "upstream status " + formatInt(int64(status)),
			Status:    status,
		}
	}
	return RetryClassification{Reason: "upstream status " + formatInt(int64(status)), Status: status}
}

// extractJSONErrorObject returns the first JSON object embedded in text, so a
// message that wraps a body in prose still parses.
func extractJSONErrorObject(text string) []byte {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return nil
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(trimmed); index++ {
		ch := trimmed[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := trimmed[start : index+1]
				if json.Valid([]byte(candidate)) {
					return []byte(candidate)
				}
				return nil
			}
		}
	}
	return nil
}

// jsonStringField reads a nested string field from a JSON object without
// requiring the caller to model every upstream error shape.
func jsonStringField(body []byte, path ...string) string {
	if len(body) == 0 || len(path) == 0 {
		return ""
	}
	var current any
	if err := json.Unmarshal(body, &current); err != nil {
		return ""
	}
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[key]
		if !ok {
			return ""
		}
	}
	switch value := current.(type) {
	case string:
		return value
	case float64:
		if value == float64(int64(value)) {
			return formatInt(int64(value))
		}
	}
	return ""
}

func formatInt(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return string(digits[index:])
}

// retryErrorMessageText renders the error payload of a stream frame for the
// classifier. It accepts the already-parsed frame JSON when available.
func retryErrorMessageText(frame []byte) string {
	text := strings.TrimSpace(string(frame))
	if text == "" {
		return ""
	}
	return text
}
