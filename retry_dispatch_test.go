package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The retry chain's ABI entry points, exercised through handleMethod the way
// the host calls them (coverage the ported suite lacked).

func retryDispatchConfig(t *testing.T, cfg Config) {
	t.Helper()
	previous := currentConfig.Load()
	currentConfig.Store(cfg)
	t.Cleanup(func() {
		if previous != nil {
			currentConfig.Store(previous)
		}
	})
}

func TestDispatchModelRouteDeclinesWhenChainInactive(t *testing.T) {
	retryDispatchConfig(t, DefaultConfig())
	payload, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.6-sol", Stream: true})
	raw, err := handleMethod(pluginabi.MethodModelRoute, payload)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.Handled {
		t.Fatalf("inactive chain claimed a request: %#v", envelope.Result)
	}
}

func TestDispatchModelRouteNonStreamDeclinesEvenWhenEnabled(t *testing.T) {
	cfg := retryTestConfig(RetryChainRow{Model: "gpt-5.6-sol", Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}}})
	cfg.RetryEnabled = true
	retryDispatchConfig(t, cfg)
	payload, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.6-sol", Stream: false})
	raw, err := handleMethod(pluginabi.MethodModelRoute, payload)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.Handled {
		t.Fatal("non-streaming request was claimed; the executor only serves streams")
	}
}

func TestDispatchExecutorIdentifierMatchesPluginID(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodExecutorIdentifier, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Identifier string `json:"identifier"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.Identifier != PluginID {
		t.Fatalf("executor identifier = %q, want plugin id %q", envelope.Result.Identifier, PluginID)
	}
}

func TestDispatchExecutorNonStreamMethodsUnsupported(t *testing.T) {
	for _, method := range []string{pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest} {
		raw, err := handleMethod(method, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if !strings.Contains(string(raw), "only serves streaming requests") {
			t.Fatalf("%s response missing streaming-only rejection: %s", method, raw)
		}
	}
}

func TestDispatchModelRouteClaimsConfiguredStreamingModel(t *testing.T) {
	cfg := retryTestConfig(RetryChainRow{Model: "gpt-5.6-sol", Fallbacks: []RetryTarget{{Model: "gpt-5.6-luna"}}})
	cfg.RetryEnabled = true
	retryDispatchConfig(t, cfg)
	payload, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "gpt-5.6-sol", Stream: true, SourceFormat: "openai-response"})
	raw, err := handleMethod(pluginabi.MethodModelRoute, payload)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Result.Handled {
		t.Fatalf("configured streaming model was not claimed: %#v", envelope.Result)
	}
}
