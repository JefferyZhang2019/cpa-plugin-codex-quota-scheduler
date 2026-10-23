package main

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// LegacyRefreshTxn is the S3 migration envelope.  It owns one coordinator
// instance lease for the complete pre-S4 refresh sequence.  The wrapped host
// makes every external operation consume an HTTP slot without submitting a
// nested intent.
type LegacyRefreshTxn struct{ refresher *QuotaRefresher }

type LegacyEffectJournal struct {
	Accounts []AccountState
	Logs     []LogEntry
	HostLogs []BufferedHostLog
}
type BufferedHostLog struct {
	Level, Message string
	Fields         map[string]any
}

func (t *LegacyRefreshTxn) RunHeld(ctx context.Context, intent Intent, held *HeldLease) OperationResult {
	result := OperationResult{Token: intent.Token, Generation: intent.Generation, Login: intent.Login, Fingerprint: intent.Fingerprint}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	payload, ok := intent.Payload.(LegacyRefreshPayload)
	auth := payload.Auth
	version := payload.AdmissionVersion
	if !ok {
		result.Err = errors.New("legacy refresh payload is not a host auth entry")
		return result
	}
	if t == nil || t.refresher == nil {
		result.Err = errors.New("legacy refresh transaction has no refresher")
		return result
	}
	if held == nil {
		result.Err = errors.New("legacy refresh transaction requires held lease")
		return result
	}
	overlay := t.refresher.state.cloneForLegacyTransaction()
	baselineLogs := len(overlay.Snapshot(t.refresher.now()).Logs)
	j := LegacyEffectJournal{}
	permit := func() bool {
		if !t.refresher.state.CPAAdmissionVersionCurrent(version) {
			return false
		}
		if t.refresher.bindings == nil {
			return true
		}
		b, ok := t.refresher.bindings.Lookup(intent.AuthID)
		return ok && ValidateWriteback(BindingVersion{Instance: b.Instance, Admission: b.Admission, Tier: TierGeneration(b.Generation), Login: b.Login, Fingerprint: b.Fingerprint}, WritebackVersion{Token: intent.Token, Login: intent.Login, Fingerprint: intent.Fingerprint})
	}
	r := QuotaRefresher{host: heldHostClient{HostClient: t.refresher.host, ctx: ctx, lease: held, journal: &j, permit: permit}, state: overlay, now: t.refresher.now, runtimeStore: t.refresher.runtimeStore, bindings: t.refresher.bindings, credentials: t.refresher.credentials, lifecycleOwner: t.refresher}
	r.txnIntent, r.txnContext, r.txnLease, r.txnPermit = &intent, ctx, held, permit
	r.refreshAuthVersionedHeld(auth, version)
	effects := overlay.legacyEffectJournal(auth.ID, baselineLogs)
	effects.HostLogs = j.HostLogs
	result.Journal = &effects
	if err := ctx.Err(); err != nil {
		result.Err = err
	}
	return result
}

type LegacyRefreshPayload struct {
	Auth             pluginapi.HostAuthFileEntry
	AdmissionVersion uint64
}

type heldHostClient struct {
	HostClient
	ctx     context.Context
	lease   *HeldLease
	journal *LegacyEffectJournal
	permit  func() bool
}

type heldCredentialHost struct {
	CredentialHost
	ctx    context.Context
	lease  *HeldLease
	permit func() bool
}

func (h heldCredentialHost) GetAuth(_ context.Context, instance AuthInstanceID) (out HostAuth, err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error {
		if h.permit != nil && !h.permit() {
			return ErrStaleExecutionToken
		}
		out, err = h.CredentialHost.GetAuth(h.ctx, instance)
		return err
	})
	return
}
func (h heldCredentialHost) SaveAuth(_ context.Context, instance AuthInstanceID, auth HostAuth) (err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error {
		if h.permit != nil && !h.permit() {
			return ErrStaleExecutionToken
		}
		err = h.CredentialHost.SaveAuth(h.ctx, instance, auth)
		return err
	})
	return
}

func (h heldHostClient) Log(level, message string, fields map[string]any) {
	h.journal.HostLogs = append(h.journal.HostLogs, BufferedHostLog{level, message, fields})
}

func (h heldHostClient) GetAuth(authIndex string) (out pluginapi.HostAuthGetResponse, err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error {
		if h.permit != nil && !h.permit() {
			return errCPAAdmissionChanged
		}
		out, err = h.HostClient.GetAuth(authIndex)
		return err
	})
	return out, err
}
func (h heldHostClient) SaveAuth(name string, raw json.RawMessage) (err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error {
		if h.permit != nil && !h.permit() {
			return errCPAAdmissionChanged
		}
		err = h.HostClient.SaveAuth(name, raw)
		return err
	})
	return err
}
func (h heldHostClient) Do(req pluginapi.HTTPRequest) (out pluginapi.HTTPResponse, err error) {
	err = h.lease.DoHTTP(h.ctx, func(context.Context) error {
		if h.permit != nil && !h.permit() {
			return errCPAAdmissionChanged
		}
		if req.URL == codexResetProbeEndpoint {
			h.lease.MarkProbeSent(h.lease.coordinator.opts.Now().Add(10 * time.Minute))
		}
		out, err = h.HostClient.Do(req)
		return err
	})
	return out, err
}

func legacyAuthInstanceID(authID string) AuthInstanceID {
	h := fnv.New64a()
	_, _ = h.Write([]byte(authID))
	v := h.Sum64()
	if v == 0 {
		v = 1
	}
	return AuthInstanceID(v)
}
