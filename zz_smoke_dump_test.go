package main

// Temporary smoke-test fixture dumper. Writes a realistic status page and its
// management JSON payload under build/smoke/ so a local mock server plus a
// real browser can walk the redesigned UI. Not part of the suite; deleted
// after the smoke pass.

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestDumpSmokeFixtures(t *testing.T) {
	if os.Getenv("SMOKE_DUMP") == "" {
		t.Skip("set SMOKE_DUMP=1 to write browser smoke fixtures")
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := NewPluginState(DefaultConfig())
	store.SetAnnotations(AnnotationState{
		Accounts: map[string]AccountAnnotation{
			"auth:main-team": {Alias: "主力账号", Notes: "运营主力，工作日优先使用", Tags: []string{"team", "paid"}, GroupID: "team-a"},
			"auth:backup-2":  {Alias: "备用二号"},
		},
		Groups: map[string]GroupAnnotation{
			"team-a": {Name: "运营组"},
		},
	})
	primary := weeklyAccount("main-team", 5, now.Add(30*time.Hour), false)
	usedPrimary := 25.0
	primary.Quota.FiveHour.UsedPercent = &usedPrimary
	primary.Quota.FiveHour.ResetAt = now.Add(3 * time.Hour)
	primary.LastSuccessAt = now
	store.UpsertQuota(primary)
	backup := weeklyAccount("backup-2", 4, now.Add(48*time.Hour), false)
	usedBackup := 100.0
	backup.Quota.FiveHour.UsedPercent = &usedBackup
	backup.Quota.FiveHour.Exhausted = true
	backup.Quota.FiveHour.ResetAt = now.Add(2 * time.Hour)
	backup.LastSuccessAt = now.Add(-2 * time.Hour)
	store.UpsertQuota(backup)
	store.RecordLog("info", "quota.refresh_success", "账号额度刷新成功", map[string]any{"auth_id": "main-team"}, now.Add(-2*time.Minute))
	store.RecordLog("info", "scheduler.selected", "请求已由插件接管", map[string]any{"auth_id": "main-team"}, now.Add(-6*time.Minute))
	store.RecordLog("warn", "quota.temporary_exhausted", "账号额度已用尽，等待重置", map[string]any{"auth_id": "backup-2"}, now.Add(-8*time.Minute))
	store.RecordLog("warn", "scheduler.fallback", "插件降级到内置调度器", map[string]any{"reason": "no ordered candidates"}, now.Add(-12*time.Minute))
	store.RecordLog("error", "quota.refresh_one_failed", "账号额度刷新失败", map[string]any{"error": "401"}, now.Add(-20*time.Minute))
	store.RecordLog("info", "quota.core_cooldown_reset", "额度恢复已确认，已同步清除 CPA 核心冷却", map[string]any{"auth_id": "backup-2"}, now.Add(-30*time.Minute))

	dir := filepath.Join("build", "smoke")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	page := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/status"}, now)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d body=%s", page.StatusCode, page.Body)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.html"), page.Body, 0o644); err != nil {
		t.Fatal(err)
	}
	api := HandleManagementRequest(store, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/codex-quota-scheduler/status", Query: url.Values{"format": []string{"json"}}}, now)
	if api.StatusCode != http.StatusOK {
		t.Fatalf("api status = %d body=%s", api.StatusCode, api.Body)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), api.Body, 0o644); err != nil {
		t.Fatal(err)
	}
}
