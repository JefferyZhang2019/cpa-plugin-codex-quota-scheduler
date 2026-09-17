package main

import "encoding/json"

// abiErrorEnvelope mirrors the plugin ABI envelope. The plugin-side envelope
// type drops http_status, and the retry chain needs that field to tell an
// upstream 429 apart from a client-side error.
type abiErrorEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *abiErrorBody   `json:"error,omitempty"`
}

type abiErrorBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// hostCallError is a failed host callback that keeps the status code the host
// attached to the failure.
type hostCallError struct {
	Code    string
	Message string
	Status  int
}

func (e *hostCallError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}
