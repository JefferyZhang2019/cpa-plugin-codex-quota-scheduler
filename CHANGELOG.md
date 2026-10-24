# Changelog

All notable changes to this project are documented here. This file follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

Earlier releases (v0.1.0 – v0.2.1) predate this file; see the
[GitHub releases](https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler/releases)
page for them.

## [v0.4.1] - 2026-10-24

- **Subpath deployments now work (#17).** The management page derives its API
  root from its own URL instead of hardcoding the domain root, so a CPA
  served behind a reverse proxy under a prefix (for example
  `https://example.com/cpa/`) reaches `/cpa/v0/management/...` correctly.
  Root-path deployments are unchanged.
- The preflight check reads the CPA version from the version endpoint's
  object payload (a v0.4.0 regression that silently aborted the check).

## [v0.4.0] - 2026-10-23

- **Migrated to the CLIProxyAPI v8 SDK.** The plugin now builds against
  `router-for-me/CLIProxyAPI/v8` (v8.0.36). The ABI surface it consumes is
  additive-only against the v7 line, and ABIVersion stays 1, so the binary
  also loads on CPA v7.3.x hosts.
- **"Refresh Quota" now clears the CPA core cooldown (#14).** A manual
  per-account refresh whose fresh quota proves remaining capacity also
  invokes `host.routing.reset_cooldown` for the credential, so a recovered
  account no longer stays hard-blocked by the core's own cooldown state.
  Hosts predating the v8 plugin ABI keep plugin-side recovery and log a
  warning instead of failing.
- **Management UI redesigned (v4).** Four hash-routed pages (Overview / Logs
  / Settings / Retry Chain), design-token stylesheet, settings regrouped
  into five cards with number+unit duration inputs (API payloads
  unchanged), a four-way segmented retry mode replacing three confusing
  toggles, per-page save isolation, JS-only account card rendering with a
  compact priority line, log level filter chips, and toast notifications.

## [v0.3.3] - 2026-10-21

- Fixed a per-request memory leak in the response stream observer
  (contributed in [#16](https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler/pull/16)):
  streams that finish without emitting a `codex.rate_limits` frame kept their
  observer and buffer backing array in the global registry forever. Observers
  are now released from the request-completion hook for every terminal
  outcome, including failed, canceled, and rejected requests.
- SDK dependency bumped to CLIProxyAPI v7.3.20 — the final v7-line build. The
  next major line migrates to the v8 SDK.

## [v0.3.2] - 2026-09-23

- Auth-failed accounts recover automatically. A 401/`invalid_grant` token
  refresh no longer parks an account forever: the credential reconcile detects
  an operator re-login (a new refresh token under the same auth file),
  immediately clears the auth-failure park, and triggers one verification
  refresh; accounts also retry at the new `auth_failure_retry_interval`
  (default 30m, editable on the settings page) as a backstop. Scheduling only
  resumes after a refresh succeeds, so recovery always carries fresh quota
  evidence.
- Fallback logs no longer misreport every delegated request as
  "no ordered candidates": `scheduler.fallback` now summarizes why each
  offered candidate was excluded (for example
  `auth_failure=2,temporary_exhausted=1`).

## [v0.3.1] - 2026-09-20

- Fixed a management-page crash introduced in v0.3.0: the settings form
  referenced a lifecycle-history input the template never rendered, so loading
  data threw "Cannot set properties of null" and hid the protected area. The
  retry-chain CPA confirmation block is rendered completely, the retry section
  stays hidden until data loads, and a template test now fails the build when
  any scripted element reference is missing.

## [v0.3.0] - 2026-09-20

- Built on the CPA v7.3 plugin SDK (schema 6): raw-JSON management responses,
  official request lifecycle events, and the QuotaProvider interface exposing
  cached Codex quota to CPA management clients.
- Quota is now observed from real response streams: a strictly read-only
  stream-chunk interceptor parses `codex.rate_limits` frames, and successful
  usage records consume the host-normalized `X-Codex-*` snapshot. Observations
  refresh the cache, defer polling, and carry window-identity evidence into
  the temporary-exhaustion reconciliation. The UI shows each account's quota
  source.
- Temporary-exhaustion markers now clear automatically when a fresh quota read
  proves an actual upstream reset (window identity changed and every known
  window has capacity), in addition to real-request success and the
  operator-confirmed manual refresh. Same-window percentages alone still never
  clear anything.
- Optional per-model retry chain with failover (Live / Shadow / Always modes)
  for retryable upstream failures before any content is committed, ported from
  doer-ee's Codex Fleet Manager (MIT). Disabled by default.
- Schedule across CPA priority tiers: with `schedule_across_priorities`
  (default on) a lower tier is selected when every higher tier is exhausted,
  instead of delegating to the built-in scheduler.
- Opt-in managed quota disable and recovery lane (429 → CPA-level disable →
  read-only polling → verified re-enable) with credential-fingerprint
  ownership, ported from dos1989's fork (MIT) and hardened.
- Probe hardening from Siriussee's fork: ambiguous sends recover without an
  unrelated wake, and a mid-probe server-side compensating reset rebases the
  baseline instead of looping in AnomalyHold. The probe model moves to
  gpt-5.6-luna.
- Management UI: account pin toggle, remembered-key reveal on auth failure,
  per-account quota source, and managed-disable status.

## [v0.2.2] - 2026-09-19

- Existing installations safely migrate their lazy-reset baselines; fresh
  installations observe the first confirmed lazy reset window before activation.
- While normal refresh is dormant, the opt-in Probe continues read-only
  observation at the quota refresh interval with a 30-minute minimum.
- A compact activation request is sent only after strict lazy-window evidence;
  after a confirmed reset, the window re-arms for the next cycle.
- Persisted state and per-window single-flight coordination preserve safe
  behavior across crashes and concurrent triggers.

## [v0.2.0] - 2026-07-15

- Availability now comes before plugin priority: an unusable high-priority
  account can no longer outrank a usable account.
- Unavailable accounts are displayed by expected recovery time, earliest first,
  with unknown recovery times last.
- The five-hour quota window is optional. Accounts remain schedulable when
  OpenAI omits it, provided a valid weekly or monthly long window is present.
- A missing or invalid long window remains Unknown and unavailable, allowing CPA
  fallback instead of making an unsafe selection.
- The active Codex account list is synchronized from CPA's authoritative auth
  roster and restricted to the highest confirmed CPA auth-priority tier.
- Reset-window activation uses a persisted, single-flight sequence that verifies
  the result after one small Codex request. It remains opt-in.
- The Management UI queue follows the same availability classes and ordering
  rules as production selection.
