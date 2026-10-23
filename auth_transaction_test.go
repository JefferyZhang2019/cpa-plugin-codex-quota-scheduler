package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestLegacyDrainSentUnknownPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	store := NewStateStore(path, OSFileHooks(), nil)
	if _, _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	r := &QuotaRefresher{runtimeStore: store, now: func() time.Time { return time.Unix(10, 0).UTC() }}
	intent := Intent{Instance: 7, Token: ExecutionToken{Instance: 7, Fence: 9}}
	until := time.Unix(100, 0).UTC()
	if err := r.persistLegacySentUnknown(intent, until); err != nil {
		t.Fatal(err)
	}
	restart := NewStateStore(path, OSFileHooks(), nil)
	state, _, err := restart.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := state.ProbeAttempts[7]
	if got.Phase != ProbeAttemptSentUnknown || got.SendFenceSeq != 9 || !got.SuppressUntil.Equal(until) {
		t.Fatalf("restart attempt=%+v", got)
	}
}

func TestLegacyProbeMarksSendOnlyAfterHTTPSlotAcquired(t *testing.T) {
	c := NewCoordinator(CoordinatorOptions{MaxHTTPSlots: 1})
	defer c.Close()
	occupy, occupied := make(chan struct{}), make(chan struct{})
	go func() { _ = c.DoHostCallback(context.Background(), func() { close(occupied); <-occupy }) }()
	<-occupied
	host := &fakeHostClient{responseByURL: map[string]pluginapi.HTTPResponse{codexResetProbeEndpoint: {StatusCode: http.StatusOK}}, startedByURL: make(chan string, 1)}
	held := &HeldLease{coordinator: c}
	done := make(chan struct{})
	go func() {
		_, _ = (heldHostClient{HostClient: host, ctx: context.Background(), lease: held, journal: &LegacyEffectJournal{}}).Do(pluginapi.HTTPRequest{Method: http.MethodPost, URL: codexResetProbeEndpoint})
		close(done)
	}()
	if sent, _ := held.probeState(); sent {
		t.Fatal("probe marked sent while waiting for slot")
	}
	close(occupy)
	<-host.startedByURL
	if sent, _ := held.probeState(); !sent {
		t.Fatal("probe not marked immediately before host call")
	}
	<-done
}

func TestMockGroupACoordinatorMigration(t *testing.T) {
	if LegacyRefreshSource != "legacy_refresh_txn" {
		t.Fatalf("source = %q", LegacyRefreshSource)
	}
}

func TestLegacyRefreshCompleteOutboundEnvelope(t *testing.T) {
	resetAt := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	now := resetAt.Add(10 * time.Minute)
	seconds := int64(fiveHourSeconds)
	idToken := makeUnsignedJWT(t, map[string]any{"chatgpt_account_id": "acct-1"})
	base := &fakeHostClient{
		authList: []pluginapi.HostAuthFileEntry{{ID: "auth-1", AuthIndex: "idx-1", Name: "auth-1.json", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"idx-1": json.RawMessage(`{"access_token":"old","refresh_token":"refresh","id_token":"` + idToken + `","account_id":"acct-1","expired":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`)},
		responseByURL: map[string]pluginapi.HTTPResponse{
			codexTokenEndpoint:      {StatusCode: http.StatusOK, Body: []byte(`{"access_token":"new","refresh_token":"new-refresh","id_token":"` + idToken + `","expires_in":3600}`)},
			chatGPTQuotaEndpoint:    {StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)},
			resetCreditsEndpoint:    {StatusCode: http.StatusOK, Body: []byte(`{"available_count":0}`)},
			codexResetProbeEndpoint: {StatusCode: http.StatusOK, Body: []byte(`{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)},
		},
	}
	host := &recordingEnvelopeHost{HostClient: base}
	cfg := DefaultConfig()
	cfg.EnableResetProbe = true
	store := NewPluginState(cfg)
	store.RecordCodexActivity(now)
	store.UpsertQuota(AccountState{AuthID: "auth-1", AuthIndex: "idx-1", Provider: "codex", Quota: ParsedQuota{FiveHour: &QuotaWindow{Kind: WindowFiveHour, LimitWindowSeconds: &seconds, ResetAt: resetAt}}, ResetProbes: map[WindowKind]ResetProbeState{WindowFiveHour: {WindowKind: WindowFiveHour, WindowSeconds: fiveHourSeconds, ResetAt: resetAt, NextCheckAt: now, Status: ResetProbeStatusPending}}, LastSuccessAt: resetAt.Add(-time.Hour)})
	store.ReplaceCPAAdmission(CPAAdmissionState{Observed: true, Priority: 1, AuthIDs: map[string]struct{}{"auth-1": {}}})
	r := NewQuotaRefresher(host, store, func() time.Time { return now })
	if err := r.RefreshDueOnce(); err != nil {
		t.Fatal(err)
	}
	want := []string{"GetAuth", "POST " + codexTokenEndpoint, "SaveAuth", "GET " + chatGPTQuotaEndpoint, "GET " + resetCreditsEndpoint}
	if got := host.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("outbound envelope = %#v, want %#v", got, want)
	}
}

type recordingEnvelopeHost struct {
	HostClient
	mu    sync.Mutex
	calls []string
}

func (h *recordingEnvelopeHost) record(call string) {
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.mu.Unlock()
}
func (h *recordingEnvelopeHost) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}
func (h *recordingEnvelopeHost) GetAuth(index string) (pluginapi.HostAuthGetResponse, error) {
	h.record("GetAuth")
	return h.HostClient.GetAuth(index)
}
func (h *recordingEnvelopeHost) SaveAuth(name string, raw json.RawMessage) error {
	h.record("SaveAuth")
	return h.HostClient.SaveAuth(name, raw)
}
func (h *recordingEnvelopeHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.record(req.Method + " " + req.URL)
	return h.HostClient.Do(req)
}
