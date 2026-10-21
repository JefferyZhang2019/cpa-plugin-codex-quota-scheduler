package main

// management_page.go carries the embedded management UI: the status page
// template, its stylesheet, and the page script. Handlers stay in
// management.go; this split keeps the view layer reviewable on its own.

import (
	"encoding/json"
	"html/template"
	"strings"
)

var statusTemplateV2 = template.Must(template.New("status-v2").Funcs(template.FuncMap{
	"join": strings.Join,
	"json": jsonForTemplate,
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Codex 额度调度器</title>
<style>
:root{
  --c-primary:#2563eb;--c-primary-soft:#eff6ff;--c-primary-deep:#1e3a8a;
  --c-bg:#f6f7f9;--c-surface:#fff;--c-success:#166534;--c-success-soft:#ecfdf5;
  --c-warn:#b45309;--c-warn-line:#fbbf24;--c-warn-soft:#fffbeb;--c-warn-text:#92400e;
  --c-danger:#b91c1c;--c-danger-fill:#dc2626;--c-text:#111827;--c-text-2:#6b7280;
  --c-border:#e5e7eb;--c-quota-bar:#2f7d5f;--c-quota-warn:#b7791f;
  --space-1:4px;--space-2:8px;--space-3:12px;--space-4:16px;--space-5:20px;--space-6:28px;
  --radius-sm:6px;--radius-md:8px;--radius-lg:10px;--radius-pill:99px;
}
*{box-sizing:border-box}
body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;color:#1f2937;background:var(--c-bg);font-size:13px}
button,input,select,textarea{font:inherit}
button{border:0;border-radius:var(--radius-sm);padding:9px 12px;background:var(--c-primary);color:#fff;cursor:pointer;font-weight:650}
button.secondary{background:#eef2ff;color:#1e40af}
button.ghost{background:#f3f4f6;color:#374151}
button:disabled{opacity:.55;cursor:not-allowed}
[hidden]{display:none!important}
code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}
.shell{display:grid;grid-template-columns:230px minmax(0,1fr);min-height:100vh}
.sidebar{background:var(--c-surface);border-right:1px solid var(--c-border);padding:var(--space-4);position:sticky;top:0;height:100vh;overflow:auto;display:grid;grid-auto-rows:min-content;gap:var(--space-4)}
.brand{display:grid;gap:var(--space-1)}
.brand h1{font-size:var(--space-5);line-height:1.2;margin:0;color:var(--c-text)}
.brand p{font-size:11px;line-height:1.45;color:var(--c-text-2);margin:0}
.nav{display:grid;gap:var(--space-1)}
.navItem{display:flex;align-items:center;gap:var(--space-2);text-align:left;background:transparent;color:#374151;font-weight:600;padding:8px 10px;border-radius:var(--radius-sm)}
.navItem:hover{background:var(--c-bg)}
.navItem.active{background:var(--c-primary);color:#fff}
.sidebarConn{border-top:1px solid var(--c-border);padding-top:var(--space-4);display:grid;gap:var(--space-3);align-content:start}
.field{display:grid;gap:var(--space-1);margin-bottom:var(--space-3)}
.field span{font-size:11px;color:#4b5563;font-weight:650}
.field input,.field select,.field textarea{width:100%;border:1px solid #d1d5db;border-radius:var(--radius-sm);background:var(--c-surface);color:var(--c-text);padding:8px 10px}
.field textarea{min-height:84px;resize:vertical}
.toggle{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3);margin-bottom:var(--space-3)}
.toggle span{font-size:13px;font-weight:650}
.setting-with-help{margin-bottom:var(--space-3)}
.setting-with-help .toggle{margin-bottom:var(--space-1)}
.setting-help{margin:0;color:var(--c-text-2);font-size:11px;line-height:1.45}
.actions{display:flex;gap:var(--space-2);flex-wrap:wrap}
.primary-actions{margin-top:var(--space-3)}
.notice{border-radius:var(--radius-md);padding:10px 11px;background:var(--c-success-soft);color:#065f46;font-size:11px;line-height:1.45;transition:opacity .35s ease}
.notice.error{background:#fef2f2;color:var(--c-danger)}
.notice.fading{opacity:0}
.staticHint{background:var(--c-primary-soft);color:var(--c-primary-deep)}
.warning{border:1px solid var(--c-warn-line);background:var(--c-warn-soft);color:#78350f;border-radius:var(--radius-md);padding:10px 11px;font-size:11px;line-height:1.45;display:grid;gap:var(--space-1)}
.warning strong{color:var(--c-warn-text);font-size:12px}
.main{padding:var(--space-5) 22px 32px;overflow:auto}
.page{display:grid;gap:var(--space-4)}
.pageHead h2{font-size:20px;margin:0;color:var(--c-text)}
.pageHead p{font-size:12px;color:var(--c-text-2);margin:var(--space-1) 0 0}
.toolbar{display:flex;align-items:flex-start;justify-content:space-between;gap:var(--space-4);margin-bottom:var(--space-4)}
.toolbar h2{font-size:22px;margin:0;color:var(--c-text)}
.toolbar p{font-size:12px;color:var(--c-text-2);margin:var(--space-1) 0 0}
.metrics{display:flex;gap:var(--space-2);flex-wrap:wrap;justify-content:flex-end}
.metric{border:1px solid var(--c-border);background:var(--c-surface);border-radius:var(--radius-sm);padding:8px 10px;font-size:11px;color:#374151}
.queue{display:grid;grid-template-columns:repeat(auto-fill,minmax(310px,1fr));gap:var(--space-3)}
.card{background:var(--c-surface);border:1px solid var(--c-border);border-radius:var(--radius-md);padding:14px;display:grid;gap:var(--space-3);box-shadow:0 1px 2px rgba(15,23,42,.04)}
.card.next{border-color:var(--c-primary);box-shadow:0 0 0 1px rgba(37,99,235,.18),0 8px 24px rgba(37,99,235,.08)}
.cardTop{display:flex;align-items:flex-start;justify-content:space-between;gap:10px}
.rank{display:inline-flex;align-items:center;justify-content:center;min-width:30px;height:30px;border-radius:var(--radius-sm);background:var(--c-text);color:#fff;font-weight:750;font-size:13px}
.identity{min-width:0;display:grid;gap:var(--space-1)}
.titleLine{display:flex;align-items:center;gap:7px;flex-wrap:wrap}
.title{font-weight:750;color:var(--c-text);overflow-wrap:anywhere}
.groupPill{border-radius:var(--radius-pill);background:#f0fdf4;color:var(--c-success);padding:3px 7px;font-size:10px}
.sub{font-size:11px;color:var(--c-text-2);overflow-wrap:anywhere}
.badges{display:flex;gap:var(--space-1);flex-wrap:wrap}
.badge{border-radius:var(--radius-pill);background:#f3f4f6;color:#374151;padding:4px 8px;font-size:11px}
.badge.ok{background:#dcfce7;color:var(--c-success)}
.badge.no{background:#fee2e2;color:var(--c-danger)}
.badge.next{background:#dbeafe;color:#1d4ed8}
.kv{display:grid;grid-template-columns:88px minmax(0,1fr);gap:var(--space-1) 10px;font-size:11px}
.kv span:nth-child(odd){color:var(--c-text-2)}
.kv span:nth-child(even){color:var(--c-text);overflow-wrap:anywhere}
.chips{display:flex;gap:var(--space-1);flex-wrap:wrap;min-height:24px}
.chip{border-radius:var(--radius-pill);background:#eef2ff;color:#3730a3;padding:4px 8px;font-size:11px}
.quotaList{display:grid;gap:10px}
.quota-row{display:grid;gap:var(--space-1)}
.quota-head{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:var(--space-2);font-size:11px;color:#374151}
.quota-title{font-weight:650;color:var(--c-text)}
.quota-reset{grid-column:1/-1;color:var(--c-text-2)}
.localTime{color:#374151}
.quota-bar{height:8px;border-radius:var(--radius-pill);background:var(--c-border);overflow:hidden}
.quota-fill{height:100%;border-radius:var(--radius-pill);background:var(--c-quota-bar)}
.quota-fill.warn{background:var(--c-quota-warn)}
.quota-fill.danger{background:var(--c-danger-fill)}
.metaLine{display:flex;gap:var(--space-1);flex-wrap:wrap}
.noteBlock{font-size:11px;line-height:1.45;color:#4b5563;background:#f9fafb;border-radius:var(--radius-sm);padding:8px 9px;display:grid;gap:3px}
.cardActions{display:flex;justify-content:flex-end;gap:var(--space-2)}
.pinAccount{display:inline-flex;align-items:center;justify-content:center;padding:9px;color:var(--c-text-2)}
.pinAccount.pinned{background:#dbeafe;color:var(--c-primary)}
.pinAccount svg{width:18px;height:18px;fill:currentColor}
.empty{background:var(--c-surface);border:1px dashed #d1d5db;border-radius:var(--radius-md);padding:var(--space-6);text-align:center;color:var(--c-text-2)}
.logs{background:var(--c-surface);border:1px solid var(--c-border);border-radius:var(--radius-md);padding:14px}
.logsHeader{display:flex;justify-content:space-between;align-items:center;gap:var(--space-3);margin-bottom:10px}
.logsHeader h2{font-size:16px;margin:0}
.logList{display:grid;gap:var(--space-2)}
.logItem{border-left:3px solid #d1d5db;padding:7px 9px;background:#f9fafb;border-radius:0 var(--radius-sm) var(--radius-sm) 0}
.logItem.info{border-left-color:var(--c-primary)}
.logItem.warn{border-left-color:var(--c-quota-warn)}
.logItem.error{border-left-color:var(--c-danger-fill)}
.logMeta{font-size:10px;color:var(--c-text-2);margin-bottom:3px}
.logMsg{font-size:11px;color:var(--c-text);line-height:1.45}
dialog{border:0;border-radius:var(--radius-md);padding:0;width:min(560px,calc(100vw - 28px));box-shadow:0 24px 64px rgba(15,23,42,.28)}
dialog::backdrop{background:rgba(15,23,42,.38)}
.dialogBody{padding:18px}
.dialogHead{display:flex;justify-content:space-between;gap:var(--space-3);align-items:flex-start;margin-bottom:14px}
.dialogHead h2{font-size:18px;margin:0;color:var(--c-text)}
.dialogHead p{font-size:11px;color:var(--c-text-2);margin:var(--space-1) 0 0}
.dialogGrid{display:grid;grid-template-columns:1fr 1fr;gap:10px}
.dialogGrid .wide{grid-column:1/-1}
.dialogActions{display:flex;justify-content:flex-end;gap:var(--space-2);margin-top:14px}
.retryChainHead{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3);margin:var(--space-1) 0 var(--space-2)}
.retryChainHead h3{margin:0;font-size:15px;color:var(--c-text)}
.retryChainRow{border:1px solid var(--c-border);border-radius:var(--radius-md);padding:var(--space-3);margin-bottom:10px;background:#fbfbfd;display:grid;gap:10px}
.retryChainRowHead{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:10px;align-items:end}
.retryChainRowHead .field{margin-bottom:0}
.retryRowActions{display:flex;gap:var(--space-2);flex-wrap:wrap}
.retryFallbacks{display:grid;gap:var(--space-2)}
.retryFallback{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:10px;align-items:end;border-top:1px dashed var(--c-border);padding-top:var(--space-2)}
.retryFallback .field{margin-bottom:0}
.retryEmpty{border:1px dashed #d1d5db;border-radius:var(--radius-md);padding:14px;color:var(--c-text-2);font-size:11px}
.retryStats{display:flex;gap:var(--space-2);flex-wrap:wrap;margin-top:var(--space-4)}
.retryStat{border:1px solid var(--c-border);background:var(--c-surface);border-radius:var(--radius-sm);padding:8px 10px;font-size:11px;display:grid;gap:3px;min-width:96px}
.retryStatLabel{color:var(--c-text-2)}
.retryStatValue{font-weight:700;color:var(--c-text);font-size:15px}
.retryChainSummary{border:1px solid var(--c-border);background:var(--c-surface);border-radius:var(--radius-sm);padding:8px 10px;font-size:11px;color:#374151;display:grid;gap:var(--space-1);flex-basis:100%}
.retryChainSummaryTitle{font-weight:650;color:var(--c-text)}
.retryChainLine{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;overflow-wrap:anywhere}
.retryCPAConfirm{margin-top:var(--space-3);padding-top:var(--space-3);border-top:1px solid #eef0f3}
.retryCPAConfirm strong{display:block;margin-bottom:var(--space-2);color:var(--c-text)}
.retryCPAConfirm table{width:100%;border-collapse:collapse;font-size:11px;margin:var(--space-2) 0 var(--space-3)}
.retryCPAConfirm th,.retryCPAConfirm td{border:1px solid var(--c-border);padding:7px 8px;text-align:left}
.retryCPAConfirm th{background:#f9fafb;color:#4b5563}
.retryCPAConfirm .diff{color:var(--c-danger);background:#fef2f2;font-weight:650}
.pageIntro{padding:var(--space-2) var(--space-3);border-radius:var(--radius-md);background:var(--c-primary-soft);color:var(--c-primary-deep);font-size:11px;line-height:1.5}
.groupGrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:var(--space-3);align-items:start}
.groupCard{border:1px solid var(--c-border);border-radius:var(--radius-lg);padding:var(--space-3);background:var(--c-surface);display:grid;gap:var(--space-2);align-content:start}
.groupCard h3{margin:0;font-size:12px;color:var(--c-text)}
.groupDesc{margin:0 0 var(--space-1);font-size:10px;color:var(--c-text-2);line-height:1.5}
.groupCard.risk .groupDesc{color:#a16207}
.groupCard.risk{border-color:var(--c-warn-line);background:var(--c-warn-soft)}
.groupCard.risk h3{color:var(--c-warn-text)}
.groupCard.risk .toggle span{color:#78350f}
.riskNote{margin:0;font-size:10px;color:var(--c-warn-text);line-height:1.45}
.durationInput{display:flex}
.durationInput input{flex:1;min-width:0;border:1px solid #d1d5db;border-right:0;border-radius:var(--radius-sm) 0 0 var(--radius-sm)}
.durationInput select{border:1px solid #d1d5db;border-radius:0 var(--radius-sm) var(--radius-sm) 0;background:#f9fafb;width:auto}
.panelActions{justify-content:flex-end}
.segmented{display:flex;border:1px solid #d1d5db;border-radius:var(--radius-md);overflow:hidden}
.segmented label{flex:1;display:flex;position:relative}
.segmented input{position:absolute;opacity:0;pointer-events:none}
.segmented span{flex:1;text-align:center;padding:6px 10px;font-size:11px;font-weight:600;border-left:1px solid #d1d5db;cursor:pointer;color:#374151;background:var(--c-surface)}
.segmented label:first-child span{border-left:0}
.segmented input:checked+span{background:var(--c-primary);color:#fff}
.segmented input:focus-visible+span{outline:2px solid var(--c-primary);outline-offset:-2px}
.advancedParams{border:1px dashed #d1d5db;border-radius:var(--radius-lg);padding:10px var(--space-3)}
.advancedParams summary{cursor:pointer;list-style:none;color:var(--c-text-2);font-size:11px;font-weight:650}
.advancedParams summary::-webkit-details-marker{display:none}
.advancedParams[open] summary{margin-bottom:var(--space-2)}
.advancedParams .field{margin-bottom:var(--space-2)}
.legacyToggles{display:none}
.priorityLine{font-size:10px;color:var(--c-text-2);text-align:right;line-height:1.5}
.priorityLine b{color:var(--c-primary)}
.quota-value{display:flex;align-items:baseline;gap:4px;font-size:11px;color:var(--c-text-2);white-space:nowrap}
.quota-value b{font-size:13px;font-weight:750;color:var(--c-text)}
.quota-value.danger b{color:var(--c-danger-fill)}
.quota-value.warn b{color:var(--c-quota-warn)}
.metaFlow{display:flex;gap:2px var(--space-2);flex-wrap:wrap;font-size:10px;color:var(--c-text-2);line-height:1.6}
.metaFlow>span+span::before{content:'·';margin-right:var(--space-2);color:#d1d5db}
.metaFlow .localTime{color:inherit}
.pinBtn{display:inline-flex;align-items:center;gap:4px;font-size:10px;padding:4px 10px;border-radius:var(--radius-pill);border:1px solid var(--c-border);background:var(--c-surface);color:#374151;font-weight:600}
.pinBtn:hover{border-color:var(--c-primary);color:var(--c-primary)}
.pinBtn.pinned{border-color:#bfdbfe;background:var(--c-primary-soft);color:var(--c-primary)}
.card.next .pinBtn.pinned{background:#dbeafe}
.logChips{display:flex;gap:var(--space-2);flex-wrap:wrap;align-items:center}
.logChip{border:1px solid var(--c-border);border-radius:var(--radius-pill);background:var(--c-surface);color:#374151;padding:4px 12px;font-size:11px;font-weight:600}
.logChip.active{background:var(--c-primary);border-color:var(--c-primary);color:#fff}
.logChip.danger{color:var(--c-danger)}
@media(max-width:860px){.shell{grid-template-columns:1fr}.sidebar{height:auto;position:relative;border-right:0;border-bottom:1px solid var(--c-border)}.toolbar{display:grid}.metrics{justify-content:flex-start}.dialogGrid{grid-template-columns:1fr}}
</style>
</head>
<body>
<div class="shell">
<aside class="sidebar">
<div class="brand"><h1 data-i18n="app.title">Codex 额度调度器</h1><p data-i18n="app.subtitle">优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。</p></div>
<nav class="nav" id="pageNav" aria-label="页面导航">
<button type="button" class="navItem" data-page="overview" data-i18n="nav.overview">账号总览</button>
<button type="button" class="navItem" data-page="logs" data-i18n="nav.logs">日志</button>
<button type="button" class="navItem" data-page="settings" data-i18n="nav.settings">调度设置</button>
<button type="button" class="navItem" data-page="retry" data-i18n="nav.retry">模型重试链</button>
</nav>
<div class="sidebarConn">
<label class="field"><span data-i18n="app.language">界面语言</span><select id="localeSelect"><option value="zh-CN">中文</option><option value="en">English</option></select></label>
<label class="field" id="managementKeyField"><span data-i18n="connection.managementKey">CPA 管理密钥</span><input id="managementKey" type="password" autocomplete="off" spellcheck="false"></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="connection.rememberManagementKey">在此浏览器中记住管理密钥</span><input id="rememberManagementKey" type="checkbox"></label><p class="setting-help" data-i18n="connection.rememberManagementKeyHelp">密钥将以未加密形式保存在浏览器本地存储中。请仅在受信任的设备上启用。</p></div>
<div class="actions primary-actions"><button id="loadData" type="button" data-i18n="actions.loadData">加载数据</button><button id="refreshQuota" type="button" class="secondary" data-i18n="actions.refreshQuota" hidden>刷新额度</button></div>
<div class="notice staticHint" data-i18n="connection.backgroundHint">只要调度器启动了，它就会在后台自动运行，无需保持页面开启。</div>
</div>
</aside>
<main class="main" id="protectedMain" hidden>
<div id="notice" class="notice" hidden></div>
<div class="warning" id="rosterLifecycleWarning" hidden><strong id="rosterLifecycleTitle">Roster lifecycle</strong><span id="rosterLifecycleBody"></span></div>
<section class="page" data-page-section="overview">
<div class="toolbar"><div><h2 data-i18n="queue.title">账号队列</h2><p data-i18n="queue.description">账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。</p></div><div class="metrics"><span class="metric"><span data-i18n="metrics.nextAccount">下一账号</span><span class="localeColon">：</span><code id="metricNextAuthID">{{if .NextAuthID}}{{.NextAuthID}}{{else}}暂无{{end}}</code></span><span class="metric"><span data-i18n="metrics.monthlyMode">Monthly</span><span class="localeColon">：</span><code id="metricMonthlyMode">{{if eq .MonthlyMode "priority"}}优先使用{{else}}按到期时间{{end}}</code></span><span class="metric"><span data-i18n="metrics.lastSelected">最近选择</span><span class="localeColon">：</span><code id="metricLastSelected">{{if .LastSelected}}{{.LastSelected}}{{else}}暂无{{end}}</code></span></div></div>
<section class="queue" aria-label="账号卡片" data-i18n-aria="queue.aria"><div class="empty">暂无账号数据。等待额度刷新后，这里会显示账号卡片。</div></section>
</section>
<section class="page" data-page-section="logs" hidden>
<div class="pageHead"><h2 data-i18n="logs.title">调度日志</h2><p data-i18n="logs.intro">最近调度事件与错误。日志保存在插件状态中，可随时导出。</p></div>
<section class="logs"><div class="logsHeader"><h2 data-i18n="logs.recent">最近记录</h2><div class="actions"><button id="refreshLogs" type="button" class="ghost" data-i18n="actions.refreshLogs">刷新日志</button><button id="exportLogs" type="button" class="ghost" data-i18n="actions.exportLogs">导出日志</button></div></div><div class="logChips" id="logChips"></div><div id="logList" class="logList"></div></section>
</section>
<section class="page" data-page-section="settings" hidden>
<div class="pageHead"><h2 data-i18n="settings.title">调度设置</h2><p data-i18n="settings.summary">默认配置已经都设置好了，正常情况下不需要手动设置。</p></div>
<div class="warning" id="resetProbeWarning" hidden><strong data-i18n="resetProbe.warningTitle">自动激活新的额度周期默认关闭</strong><span data-i18n="resetProbe.warningBody">开启后，调度器会在额度重置时间已到但新周期尚未生成时，发送一次极小的 Codex 请求尝试激活新周期。</span></div>
<div id="settingsPanel" class="panelBody" hidden>
<div class="pageIntro" data-i18n="settings.summary">默认配置已经都设置好了，正常情况下不需要手动设置。</div>
<div class="groupGrid">
<section class="groupCard">
<h3 data-i18n="settings.groupGeneral">⚙ 常规</h3>
<p class="groupDesc" data-i18n="settings.groupGeneralDesc">决定调度器何时接管请求，以及账号之间如何排序。</p>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.handleEnabled">启用调度接管</span><input id="handleEnabled" type="checkbox"></label><p class="setting-help" data-i18n="settings.handleEnabledHelp">开启后，每次 Codex 请求由本插件按额度可用性选择账号；关闭则完全交回 CPA 内置调度。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.acrossPriorities">更高优先级耗尽时使用较低优先级账号</span><input id="scheduleAcrossPriorities" type="checkbox"></label><p class="setting-help" data-i18n="settings.acrossPrioritiesHelp">开启后，所有 CPA 优先级层级的账号都会纳入调度；更高层级只要还有可用账号就一定优先，仅在更高层级全部不可用时才会选用低层级。关闭则保持只调度最高优先级层。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.monthlyMode">月度账号使用方式</span><select id="monthlyMode"><option value="expiry_order" data-i18n="settings.expiryOrder">按到期时间排序</option><option value="priority" data-i18n="settings.monthlyPriority">优先使用月度账号</option></select></label><p class="setting-help" data-i18n="settings.monthlyModeHelp">月度（Monthly）账号的额度按月重置。「按到期时间排序」先用重置时间最早的月度账号，避免月底额度浪费；「优先使用月度账号」总是先用完月度账号，再动周额度账号。</p></div>
</section>
<section class="groupCard">
<h3 data-i18n="settings.groupRefresh">🔄 额度刷新</h3>
<p class="groupDesc" data-i18n="settings.groupRefreshDesc">控制插件多久向 Codex 查询一次各账号的额度，以及缓存何时算过期。</p>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.refreshInterval">额度刷新间隔</span><span class="durationInput"><input id="refreshInterval" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="refreshInterval" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.refreshIntervalHelp">每隔多久查询一次全部账号的额度。真实请求返回的额度信息会自动推迟下一轮查询。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.staleAfter">缓存过期判定</span><span class="durationInput"><input id="staleAfter" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="staleAfter" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.staleAfterHelp">超过此时长没有额度数据的账号视为「过期」，优先安排刷新。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.refreshActiveWindow">活跃刷新窗口</span><span class="durationInput"><input id="refreshActiveWindow" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="refreshActiveWindow" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.refreshActiveWindowHelp">最近这段时间内有过 Codex 请求时按刷新间隔执行；空闲后降频，节省额度与请求。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.refreshAfterResetDelay">重置后刷新延迟</span><span class="durationInput"><input id="refreshAfterResetDelay" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="refreshAfterResetDelay" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.refreshAfterResetDelayHelp">额度窗口的重置时间点过后，等多久再查询，给上游留出生成新窗口的时间。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.authFailureRetryInterval">认证失败重试间隔</span><span class="durationInput"><input id="authFailureRetryInterval" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="authFailureRetryInterval" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.authFailureRetryIntervalHelp">登录失效（401）的账号按此间隔低频重试，避免频繁撞无效凭证。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.refreshRetryDelays">刷新失败重试间隔</span><input id="refreshRetryDelays" spellcheck="false" placeholder="1m,5m,15m"></label><p class="setting-help" data-i18n="settings.refreshRetryDelaysHelp">额度查询失败后的重试间隔序列（逗号分隔）；全部用完后等待下一个刷新周期。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.refreshOnStartup">启动时刷新额度</span><input id="refreshOnStartup" type="checkbox"></label><p class="setting-help" data-i18n="settings.refreshOnStartupHelp">CPA 启动时立即刷新一轮额度；关闭则等首个请求触发。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.maxConcurrency">最大并发刷新</span><input id="maxConcurrency" type="number" min="1" step="1"></label><p class="setting-help" data-i18n="settings.maxConcurrencyHelp">同时查询额度的最大账号数。账号很多时可适当调大，但过高可能触发上游限流。</p></div>
</section>
<section class="groupCard risk">
<h3 data-i18n="settings.groupRecovery">⚠ 失败与恢复</h3>
<p class="groupDesc" data-i18n="settings.groupRecoveryDesc">账号出错或额度耗尽后的自动处理。⚠ 标记的选项有副作用，改动前请读说明。</p>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.usageFeedback">失败反馈标记额度耗尽</span><input id="usageFeedback" type="checkbox"></label><p class="setting-help" data-i18n="settings.usageFeedbackHelp">上游确认额度耗尽（usage_limit_reached）时立即把账号标记为不可用，直到窗口重置。关闭后只能靠定时刷新发现耗尽，调度会多撞几次 429。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.enableResetProbe">自动激活新的额度周期 ⚠</span><input id="enableResetProbe" name="enable_reset_probe" type="checkbox"></label><p class="setting-help" data-i18n="settings.enableResetProbeHelp">额度重置时间已到但新窗口迟迟未生成时，发送一次极小的 Codex 请求尝试「激活」新周期。其余时间只做只读检查；可能消耗少量额度。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.provisionalProbe">账号列表未确认时仍允许额度探测 ⚠</span><input id="probeOnProvisionalRoster" name="probe_on_provisional_roster" type="checkbox"></label><p class="setting-help" data-i18n="settings.provisionalProbeHelp">CPA 暂时无法确认当前账号及优先级时，允许插件用最近保存的账号列表执行上面的探测。每次都会重新验证凭据，但无法保证账号未被删除或调整。通常应保持关闭。</p></div>
<div class="setting-with-help"><label class="toggle"><span data-i18n="settings.managedDisable">额度耗尽时在 CPA 层停用账号并自动恢复 ⚠</span><input id="enableManagedQuotaDisable" type="checkbox"></label><p class="setting-help" data-i18n="settings.managedDisableHelp">确认额度耗尽时把该账号的 CPA 凭据标记为 disabled，之后按刷新间隔只读检查，两个额度窗口都可用时才自动恢复。人工手动停用的账号绝不会被改动。开启后插件会写宿主的认证文件。</p></div>
<p class="riskNote" data-i18n="settings.riskNote">⚠ 后两项为高风险操作（额度探测使用过期账号列表、写宿主认证文件），保持默认关闭，除非明确需要。</p>
</section>
<section class="groupCard">
<h3 data-i18n="settings.groupCircuit">🔒 熔断器</h3>
<p class="groupDesc" data-i18n="settings.groupCircuitDesc">账号连续失败后临时停用一段时间，避免持续向故障账号发请求。</p>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.circuitFailureThreshold">熔断失败阈值</span><input id="circuitFailureThreshold" type="number" min="1" step="1"></label><p class="setting-help" data-i18n="settings.circuitFailureThresholdHelp">连续失败达到该次数后进入熔断，暂时不再选择该账号。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.circuitOpenDuration">熔断等待时间</span><span class="durationInput"><input id="circuitOpenDuration" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="circuitOpenDuration" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.circuitOpenDurationHelp">熔断后等待多久，才允许半开试探。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.circuitHalfOpenSuccessThreshold">半开恢复成功次数</span><input id="circuitHalfOpenSuccessThreshold" type="number" min="1" step="1"></label><p class="setting-help" data-i18n="settings.circuitHalfOpenSuccessThresholdHelp">半开试探期成功该次数后，账号完全恢复参与调度。</p></div>
</section>
<section class="groupCard">
<h3 data-i18n="settings.groupLog">📊 日志与历史</h3>
<p class="groupDesc" data-i18n="settings.groupLogDesc">插件日志与托管停用历史的容量上限，超出后丢弃最旧记录。</p>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.maxLogEntries">最大日志条数</span><input id="maxLogEntries" type="number" min="1" step="1"></label><p class="setting-help" data-i18n="settings.maxLogEntriesHelp">面板与导出中保留的最大日志条数。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.logRetention">日志保留时间</span><span class="durationInput"><input id="logRetention" data-duration-field spellcheck="false" inputmode="decimal"><select data-duration-unit="logRetention" aria-label="单位"><option value="m" data-i18n="unit.minutes">分钟</option><option value="h" data-i18n="unit.hours">小时</option><option value="d" data-i18n="unit.days">天</option><option value="raw" data-i18n="unit.raw">原始</option></select></span></label><p class="setting-help" data-i18n="settings.logRetentionHelp">日志的最长保留时间，超期的记录在刷新时清理。</p></div>
<div class="setting-with-help"><label class="field"><span data-i18n="settings.lifecycleEventLimit">托管停用历史条数上限</span><input id="lifecycleEventLimit" type="number" min="1" step="1"></label><p class="setting-help" data-i18n="settings.lifecycleEventLimitHelp">「CPA 层停用并自动恢复」的历史最多记录条数，用于排查自动停用原因。</p></div>
</section>
</div>
<div class="actions panelActions"><button id="exportConfig" type="button" class="ghost" data-i18n="actions.exportConfig">导出配置</button><button id="importConfig" type="button" class="ghost" data-i18n="actions.importConfig">导入配置</button><input id="importFile" type="file" accept="application/json,.json" hidden><button id="saveSettings" type="button" data-i18n="actions.saveSettings">保存设置</button></div>
</div>
</section>
<section class="page" data-page-section="retry" hidden>
<div class="pageHead"><h2 data-i18n="retry.title">模型重试链</h2><p data-i18n="retry.summary">上游在内容尚未发给客户端之前返回容量不足、超载或 429/502/503 时，自动改用链中的下一个模型重试。</p></div>
<div id="retryPanelWrap" hidden><div id="retryPanel">
<section class="groupCard">
<h3 data-i18n="retry.modeTitle">运行模式</h3>
<div class="segmented" role="radiogroup" aria-label="重试运行模式">
<label><input type="radio" name="retryMode" value="off"><span data-i18n="retry.modeOff">关闭</span></label>
<label><input type="radio" name="retryMode" value="shadow"><span data-i18n="retry.modeShadow">影子</span></label>
<label><input type="radio" name="retryMode" value="live"><span data-i18n="retry.modeLive">生效</span></label>
<label><input type="radio" name="retryMode" value="always"><span data-i18n="retry.modeAlways">全局</span></label>
</div>
<p class="setting-help" data-i18n="retry.modeHelp">影子=只记录会重试的情况，不真正重试（灰度观察）· 生效=仅链内模型重试 · 全局=所有模型都重试。四选一，替代原来的三个开关。</p>
</section>
<section class="groupCard">
<div class="retryChainHead"><h3 data-i18n="retry.chainTitle">重试链</h3><div class="actions"><button id="retryAddRow" type="button" class="ghost" data-i18n="retry.addRow">添加模型</button><button id="retryReloadCatalog" type="button" class="ghost" data-i18n="retry.reloadCatalog">载入模型列表</button></div></div>
<p class="setting-help" data-i18n="retry.chainHelp">左列是客户端请求的模型；右侧按顺序填备用目标。提供方留空时由 CPA 自动选择，填写时使用 CPA 的提供方键，例如 codex、openai、openai-compatible-zed2api。只对开启了「启用模型重试链」且出现在这里的模型生效。</p>
<div id="retryChainEditor"></div>
<p class="setting-help" id="retryCatalogStatus"></p>
</section>
<section class="groupCard">
<h3 data-i18n="retry.cpaTitle">CPA 前置检查</h3>
<p class="setting-help" id="retryCPAStatus"></p>
<div class="actions"><button id="retryCheckCPA" type="button" class="ghost" data-i18n="retry.checkCPA">检查并修复 CPA 重试设置</button></div>
<div class="retryCPAConfirm" id="retryCPAConfirm" hidden><strong data-i18n="retry.cpaConfirmTitle">确认修改 CPA 重试设置</strong><p id="retryCPAConfirmBody"></p><div class="actions"><button id="retryCPAConfirmYes" type="button" data-i18n="retry.cpaConfirmYes">应用推荐设置</button><button id="retryCPAConfirmNo" type="button" class="ghost" data-i18n="retry.cpaConfirmNo">取消</button></div></div>
</section>
<details class="advancedParams">
<summary data-i18n="retry.advancedParams">▸ 高级参数</summary>
<p class="setting-help" data-i18n="retry.advancedParamsHint">默认值适合绝大多数场景；展开后才显示完整表单。</p>
<label class="field"><span data-i18n="retry.maxAttempts">最大尝试次数（含首次）</span><input id="retryMaxAttempts" type="number" min="1" max="16" step="1"></label>
<label class="field"><span data-i18n="retry.maxFrames">保留帧上限</span><input id="retryMaxFrames" type="number" min="1" step="1"></label>
<label class="field"><span data-i18n="retry.stallTimeout">单次静默超时</span><input id="retryStallTimeout" spellcheck="false" placeholder="60s"></label>
<label class="field"><span data-i18n="retry.holdTimeout">等待可提交内容上限</span><input id="retryHoldTimeout" spellcheck="false" placeholder="90s"></label>
<label class="field"><span data-i18n="retry.chainDeadline">整条链总时限</span><input id="retryChainDeadline" spellcheck="false" placeholder="240s"></label>
<label class="field"><span data-i18n="retry.maxBytes">保留字节上限</span><input id="retryMaxBytes" spellcheck="false" placeholder="8MB"></label>
<div class="setting-with-help"><label class="toggle"><span data-i18n="retry.stripReasoning">切换模型时移除加密推理内容</span><input id="retryStripReasoning" type="checkbox"></label><p class="setting-help" data-i18n="retry.stripReasoningHelp">Codex 的加密推理条目由原模型签名，换模型后无法验证。保留开启可以避免换模型后请求被上游拒绝。</p></div>
</details>
<section class="groupCard">
<div class="retryChainHead"><h3 data-i18n="retry.statsTitle">运行记录</h3><div class="actions"><button id="retryRefreshStats" type="button" class="ghost" data-i18n="retry.refreshStats">刷新记录</button></div></div>
<div id="retryStats" class="retryStats"></div>
<div id="retryEvents" class="logList"></div>
</section>
<div class="actions panelActions"><button id="retrySave" type="button" data-i18n="retry.saveButton">保存重试链设置</button></div>
<datalist id="retryProviderOptions"></datalist>
<datalist id="retryModelOptions"></datalist>
<div class="legacyToggles"><label class="toggle"><span data-i18n="retry.enabled">启用模型重试链</span><input id="retryEnabled" type="checkbox"></label><label class="toggle"><span data-i18n="retry.shadow">影子模式（只记录会重试的情况，不真正重试）</span><input id="retryShadow" type="checkbox"></label><label class="toggle wide"><span data-i18n="retry.always">全局重试（所有模型）</span><input id="retryAlways" type="checkbox"></label></div>
</div></div>
</section>
</main>
</div>
<dialog id="editDialog"><form method="dialog" class="dialogBody"><div class="dialogHead"><div><h2 data-i18n="edit.title">编辑账号</h2><p id="editAuthID"></p></div><button type="button" id="closeDialog" class="ghost" data-i18n="actions.close">关闭</button></div><div class="dialogGrid"><label class="field"><span data-i18n="edit.alias">别名</span><input id="editAlias"></label><label class="field"><span data-i18n="account.schedulerPriority">插件优先级</span><input id="editSchedulerPriority" type="number" step="1" value="0"></label><label class="field"><span data-i18n="edit.groupID">分组 ID</span><input id="editGroupID" placeholder="team-a"></label><label class="field"><span data-i18n="edit.groupName">分组名称</span><input id="editGroupName"></label><label class="field"><span data-i18n="edit.tags">标签</span><input id="editTags" placeholder="team, paid"></label><label class="field wide"><span data-i18n="edit.notes">账号备注</span><textarea id="editNotes"></textarea></label><label class="field wide"><span data-i18n="edit.groupNotes">分组备注</span><textarea id="editGroupNotes"></textarea></label></div><div class="dialogActions"><button type="button" id="saveAccount" class="secondary" data-i18n="actions.saveAccount">保存账号</button><button type="button" id="cancelEdit" class="ghost" data-i18n="actions.cancel">取消</button></div></form></dialog>
<script>
let STATUS={{json .}};
const MANAGEMENT_BASE='/v0/management/plugins/codex-quota-scheduler';
const LOCALE_STORAGE_KEY='codex-quota-scheduler-locale-v1';
const MANAGEMENT_KEY_STORAGE_KEY='codex-quota-scheduler-management-key-v1';
const TRANSLATIONS={
  en:{
    'nav.retry':'Retry Chain','nav.overview':'Overview','nav.logs':'Logs','nav.settings':'Scheduler Settings','settings.groupGeneral':'General','settings.groupGeneralDesc':'When the scheduler takes over requests and how accounts are ordered.','settings.groupRefresh':'Quota Refresh','settings.groupRefreshDesc':'How often the plugin queries Codex for each account quota, and when cached data counts as stale.','settings.groupRecovery':'Failure & Recovery','settings.groupRecoveryDesc':'Automatic handling of failing or exhausted accounts. ⚠ options have side effects; read the notes before changing them.','settings.groupCircuit':'Circuit Breaker','settings.groupCircuitDesc':'Temporarily parks an account after repeated failures instead of keeping sending requests to it.','settings.groupLog':'Logs & History','settings.groupLogDesc':'Capacity limits for plugin logs and the managed-disable history; oldest records are dropped first.','settings.handleEnabledHelp':'On: this plugin picks the account for every Codex request by quota usability. Off: scheduling falls back entirely to the CPA built-in.','settings.monthlyModeHelp':'Monthly accounts reset per calendar month. Expiry order uses the account that resets soonest first, so quota is not wasted at month end; Prefer monthly always exhausts monthly accounts before touching weekly ones.','settings.refreshIntervalHelp':'How often all account quotas are polled. Quota evidence observed on real responses automatically postpones the next poll.','settings.staleAfterHelp':'Accounts without quota data for longer than this are treated as stale and scheduled for refresh first.','settings.refreshActiveWindowHelp':'While a Codex request happened within this window, refresh runs at the interval above; when idle it slows down to save quota.','settings.refreshAfterResetDelayHelp':'After a quota window resets, wait this long before querying so the upstream has time to open the new window.','settings.authFailureRetryIntervalHelp':'Accounts whose login failed (401) retry at this low frequency instead of hammering dead credentials.','settings.refreshRetryDelaysHelp':'Retry backoff sequence after a failed quota query (comma separated); once exhausted, wait for the next refresh cycle.','settings.refreshOnStartupHelp':'Refresh one round of quotas when CPA starts; off means waiting for the first request.','settings.maxConcurrencyHelp':'Maximum accounts queried in parallel. Raise for many accounts, but too high may trip upstream rate limits.','settings.usageFeedbackHelp':'When the upstream confirms exhaustion (usage_limit_reached), mark the account unavailable immediately until the window resets. Off relies on polling only, so scheduling hits a few extra 429s.','settings.circuitFailureThresholdHelp':'Consecutive failures needed before the circuit opens and the account is skipped.','settings.circuitOpenDurationHelp':'How long the circuit stays open before half-open probing is allowed.','settings.circuitHalfOpenSuccessThresholdHelp':'Successful half-open probes needed to fully restore the account.','settings.maxLogEntriesHelp':'Maximum log entries kept for the panel and export.','settings.logRetentionHelp':'How long log records are kept before cleanup.','settings.lifecycleEventLimitHelp':'Maximum entries of the disable-and-recover history, used to audit automatic disables.','settings.riskNote':'⚠ The last two options are high-risk (probing on a stale roster, writing host auth files). Keep them off unless you specifically need them.','unit.minutes':'minutes','unit.hours':'hours','unit.days':'days','unit.raw':'raw','logs.intro':'Recent scheduler events and errors. Logs are kept in plugin state and can be exported anytime.','logs.recent':'Recent entries','logs.filterAll':'All','logs.filterInfo':'ℹ info','logs.filterWarn':'⚠ warn','logs.filterError':'✕ error','retry.title':'Model Retry Chain','retry.summary':'When an upstream reports capacity, overload, or 429/502/503 before any content reached the client, the request is retried against the next model in the chain.','retry.enabled':'Enable model retry chain','retry.shadow':'Shadow mode (record only, never retry)','retry.shadowHelp':'Shadow mode forwards every frame exactly as today and only records the requests a live chain would have retried, so you can watch it before enabling.','retry.chainTitle':'Retry chain','retry.chainHelp':'The left column is the model the client asks for; list the fallback targets in order on the right. Leave the provider empty to let CPA choose it, or use a CPA provider key such as codex, openai, or openai-compatible-zed2api. Only listed models with the chain enabled are affected.','retry.addRow':'Add model','retry.reloadCatalog':'Load model list','retry.rowModel':'Requested model','retry.provider':'Provider (optional)','retry.fallback':'Fallback','retry.addFallback':'Add fallback','retry.removeRow':'Remove model','retry.remove':'Remove','retry.maxAttempts':'Maximum attempts (including the first)','retry.stallTimeout':'Per-attempt silence timeout','retry.holdTimeout':'Hold budget before first commit','retry.chainDeadline':'Whole-chain deadline','retry.maxFrames':'Buffered frame limit','retry.maxBytes':'Buffered byte limit','retry.stripReasoning':'Drop encrypted reasoning when the model changes','retry.stripReasoningHelp':'Codex encrypted reasoning items are signed by the original model and cannot be verified by another one. Leaving this on avoids upstream rejections after a model switch.','retry.refreshStats':'Refresh records','retry.catalogLoading':'Loading provider and model list...','retry.catalogLoaded':'Provider and model list loaded.','retry.catalogFailed':'Could not load the provider and model list; type the values manually.','retry.catalogEmpty':'No providers reported; type the values manually.','retry.chainEmpty':'No retry chain configured. Add a model to start.','retry.statsTitle':'Retry records','retry.eventsTitle':'Recent attempts','retry.mode':'Mode','retry.modeOff':'Off','retry.modeLive':'Live','retry.modeShadow':'Shadow','retry.modeAlways':'Always','retry.modeTitle':'Run mode','retry.modeHelp':'Shadow = record only, never retry (canary observation) · Live = only models in the chain · Always = retry every model. One choice replaces the previous three switches.','retry.cpaTitle':'CPA preflight','retry.advancedParams':'▸ Advanced parameters','retry.advancedParamsHint':'Defaults fit most setups; expand for the full form.','retry.saveButton':'Save retry chain settings','retry.requests':'Requests','retry.retries':'Retries','retry.succeeded':'Succeeded','retry.failed':'Failed','retry.shadowHits':'Would have retried','retry.noEvents':'No retry activity recorded yet.','retry.attempt':'Attempt','retry.outcome':'Outcome','retry.invalidRow':'Every chain row needs a model and at least one fallback with a model.','retry.error.attempts':'Maximum attempts must be between 1 and 16.','retry.error.frames':'Buffered frame limit must be a whole number of at least 1.','retry.error.size':'Buffered byte limit must be a positive size such as 8MB.','retry.error.duration':'Timeouts must be positive durations such as 60s, 90s, or 240s.','notice.retrySaved':'Retry chain saved.',
    'app.title':'Codex Quota Scheduler','app.subtitle':'Optimized Fill First scheduling. Configuration, aliases, groups, tags, and notes are saved in the plugin state file.','app.language':'Language','connection.managementKey':'CPA management key','connection.rememberManagementKey':'Remember management key in this browser','connection.rememberManagementKeyHelp':'The key will be stored unencrypted in browser local storage. Enable this only on a trusted device.','connection.backgroundHint':'Once the scheduler is enabled, it runs in the background. This page does not need to stay open.',
    'resetProbe.warningTitle':'Automatic reset probe is off by default','resetProbe.warningBody':'Only after you check the box will the scheduler send one tiny Codex request when a reset looks lazy, nudging the next quota window to start.',
    'settings.title':'Scheduler Settings','settings.summary':'Default configuration is ready; normally no manual changes are needed.','settings.handleEnabled':'Enable scheduler takeover','settings.usageFeedback':'Mark quota exhausted from failure feedback','settings.enableResetProbe':'Enable automatic reset probe','settings.enableResetProbeHelp':'Probe performs read-only checks at the quota refresh interval with a 30-minute minimum, even while normal refresh is dormant, and sends one tiny request only after detecting a lazy reset window. This may consume a small amount of quota.','settings.provisionalProbe':'Allow quota probes when the account roster is unconfirmed (high risk)','settings.provisionalProbeHelp':'When CPA temporarily cannot confirm the current accounts and priorities, allow the plugin to use the most recently saved account roster for quota reset probes. Account credentials are revalidated every time, but the plugin still cannot guarantee that accounts have not been removed or reprioritized. This should normally remain off.','settings.monthlyMode':'Monthly mode','settings.acrossPriorities':'Use lower-priority accounts when higher tiers are exhausted','settings.acrossPrioritiesHelp':'When enabled, accounts from every CPA priority tier join scheduling: a higher tier always wins while it has a selectable account, and lower tiers are only reached when higher ones are all unavailable. Disable to schedule only the highest tier.','settings.managedDisable':'Disable accounts in CPA on quota exhaustion and recover automatically','settings.managedDisableHelp':'On a confirmed usage_limit_reached the account CPA credential is marked disabled; the plugin then checks quota read-only at the refresh interval and re-enables only when both quota windows are proven usable. Manually disabled accounts are never touched. Off by default; enabling lets the plugin write host auth files.','settings.expiryOrder':'Sort by expiry time','settings.monthlyPriority':'Prefer Monthly','settings.refreshInterval':'Quota refresh interval','settings.staleAfter':'Stale cache threshold','settings.refreshActiveWindow':'Refresh active window','settings.refreshAfterResetDelay':'Refresh after reset delay','settings.refreshRetryDelays':'Refresh retry delays','settings.authFailureRetryInterval':'Auth-failure retry interval','settings.refreshOnStartup':'Refresh on startup','settings.maxConcurrency':'Max refresh concurrency','settings.circuitFailureThreshold':'Circuit failure threshold','settings.circuitOpenDuration':'Circuit open duration','settings.circuitHalfOpenSuccessThreshold':'Half-open recovery successes','settings.maxLogEntries':'Max log entries','settings.lifecycleEventLimit':'Managed-disable history limit','settings.logRetention':'Log retention',
    'actions.loadData':'Load Data','actions.saveSettings':'Save Settings','actions.refreshQuota':'Refresh Quota','actions.exportConfig':'Export Config','actions.importConfig':'Import Config','actions.refreshLogs':'Refresh Logs','actions.exportLogs':'Export Logs','actions.close':'Close','actions.saveAccount':'Save Account','actions.cancel':'Cancel','actions.pinAccount':'Pin account','actions.unpinAccount':'Unpin account',
    'queue.title':'Account Queue','queue.description':'Account cards are sorted by the current scheduler priority. The first available account is preferred for the next Codex request.','queue.aria':'Account cards','metrics.nextAccount':'Next account','metrics.monthlyMode':'Monthly','metrics.lastSelected':'Last selected',
    'logs.title':'Scheduler Logs','logs.empty':'No logs yet. Send a request or refresh quota manually to show records here.',
    'edit.title':'Edit Account','edit.alias':'Alias','account.schedulerPriority':'Plugin priority','edit.groupID':'Group ID','edit.groupName':'Group name','edit.tags':'Tags','edit.notes':'Account notes','edit.groupNotes':'Group notes',
    'notice.settingsSaved':'Settings saved.','notice.statusLoaded':'Current settings loaded. Review them, then save again.','notice.refreshRequested':'Background quota refresh requested.','notice.accountSaved':'Account card saved.','notice.accountPinned':'Account pinned with plugin priority 999.','actions.pinAccountHelp':'Pin this account to the top of the scheduling queue.','actions.unpinAccountHelp':'Release the pin and return to quota-based scheduling.','notice.accountUnpinned':'Account unpinned and plugin priority reset to 0.','notice.refreshOneRequested':'Quota refresh requested for this account.','notice.configExported':'Configuration exported.','notice.logsExported':'Logs exported.','notice.configImported':'Configuration imported.','error.requestFailed':'Request failed: {status}','error.managementKeyRequired':'CPA management key is required','error.schedulerPriorityInteger':'Plugin priority must be a safe integer.',
    'log.ui.refresh_requested':'UI requested quota refresh','log.ui.settings_saved':'UI saved scheduler settings','log.ui.refresh_one_requested':'UI requested one account quota refresh','log.ui.config_exported':'UI exported plugin configuration','log.ui.config_imported':'UI imported plugin configuration','log.ui.account_saved':'UI saved account card','log.ui.group_saved':'UI saved account group','log.scheduler.selected':'Request handled by plugin','log.scheduler.fallback':'Plugin delegated to the built-in scheduler','log.scheduler.unhandled':'Request was not handled by the plugin','log.scheduler.handled':'Plugin handled the request without selecting an account','log.quota.refresh_success':'Account quota refreshed successfully','log.quota.refresh_failed':'Account quota refresh failed','log.quota.refresh_one_failed':'Single-account quota refresh failed','log.quota.refresh_requested':'Background quota refresh requested','log.quota.refresh_one_requested':'Single-account quota refresh requested','log.quota.temporary_exhausted':'Account quota exhausted; waiting for reset','log.quota.temporary_recovered':'Manual refresh verified recovery; temporary exhaustion cleared','log.quota.manual_refresh_override':'Manual refresh overrode the host-side disabled/unavailable flag','log.quota.core_cooldown_reset':'Verified recovery also cleared the CPA core cooldown','log.quota.core_cooldown_reset_unavailable':'This CPA version cannot clear the core cooldown; only the plugin-side recovery applied','log.circuit.success':'Successful request updated the circuit state','log.credential.external_login_unparked':'Re-login detected; auth-failure park cleared and a verification refresh was triggered','log.probe.precheck_started':'Checking a suspected lazy quota window','log.probe.activation_sent':'Tiny activation request sent','log.probe.verified':'Quota-window activation verified','log.probe.failed':'Quota-window probe failed and was handled safely'
  },
  'zh-CN':{
    'nav.retry':'模型重试链','retry.title':'模型重试链','retry.summary':'上游在内容尚未发给客户端之前返回容量不足、超载或 429/502/503 时，自动改用链中的下一个模型重试。','retry.enabled':'启用模型重试链','retry.shadow':'影子模式（只记录，不真正重试）','retry.shadowHelp':'影子模式完全按现有路径转发，只把「本应重试」的请求写入下面的记录，便于先观察再启用。','retry.chainTitle':'重试链','retry.chainHelp':'左列是客户端请求的模型；右侧按顺序填备用目标。提供方留空时由 CPA 自动选择，填写时使用 CPA 的提供方键，例如 codex、openai、openai-compatible-zed2api。只对开启了「启用模型重试链」且出现在这里的模型生效。','retry.addRow':'添加模型','retry.reloadCatalog':'载入模型列表','retry.rowModel':'请求的模型','retry.provider':'提供方（可留空）','retry.fallback':'备用目标','retry.addFallback':'添加备用','retry.removeRow':'删除该模型','retry.remove':'删除','retry.maxAttempts':'最大尝试次数（含首次）','retry.stallTimeout':'单次静默超时','retry.holdTimeout':'等待可提交内容上限','retry.chainDeadline':'整条链总时限','retry.maxFrames':'保留帧上限','retry.maxBytes':'保留字节上限','retry.stripReasoning':'切换模型时移除加密推理内容','retry.stripReasoningHelp':'Codex 的加密推理条目由原模型签名，换模型后无法验证。保留开启可以避免换模型后请求被上游拒绝。','retry.refreshStats':'刷新记录','retry.catalogLoading':'正在载入提供方与模型列表…','retry.catalogLoaded':'已载入提供方与模型列表。','retry.catalogFailed':'无法载入提供方与模型列表，请手动填写。','retry.catalogEmpty':'没有读到提供方，请手动填写。','retry.chainEmpty':'尚未配置重试链。添加一个模型即可开始。','retry.statsTitle':'重试记录','retry.eventsTitle':'最近尝试','retry.mode':'模式','retry.modeOff':'关闭','retry.modeLive':'生效','retry.modeShadow':'影子','retry.requests':'请求数','retry.retries':'重试次数','retry.succeeded':'成功','retry.failed':'失败','retry.shadowHits':'本应重试','retry.noEvents':'暂无重试记录。','retry.attempt':'尝试','retry.outcome':'结果','retry.invalidRow':'每个模型至少要有一个填了模型的备用目标。','retry.error.attempts':'最大尝试次数必须在 1 到 16 之间。','retry.error.frames':'保留帧上限必须是至少为 1 的整数。','retry.error.size':'保留字节上限必须是正数，例如 8MB。','retry.error.duration':'超时必须是正的时间段，例如 60s、90s 或 240s。','notice.retrySaved':'重试链已保存。','nav.overview':'账号总览','nav.logs':'日志','nav.settings':'调度设置','logs.intro':'最近调度事件与错误。日志保存在插件状态中，可随时导出。','logs.recent':'最近记录','logs.filterAll':'全部','logs.filterInfo':'ℹ info','logs.filterWarn':'⚠ warn','logs.filterError':'✕ error','settings.groupGeneral':'⚙ 常规','settings.groupRefresh':'🔄 额度刷新','settings.groupRecovery':'⚠ 失败与恢复','settings.groupCircuit':'🔒 熔断器','settings.groupLog':'📊 日志与历史','settings.riskNote':'⚠ 后两项为高风险操作（额度探测使用过期账号列表、写宿主认证文件），保持默认关闭，除非明确需要。','unit.minutes':'分钟','unit.hours':'小时','unit.days':'天','unit.raw':'原始','retry.modeAlways':'全局','retry.modeTitle':'运行模式','retry.modeHelp':'影子=只记录会重试的情况，不真正重试（灰度观察）· 生效=仅链内模型重试 · 全局=所有模型都重试。四选一，替代原来的三个开关。','retry.cpaTitle':'CPA 前置检查','retry.advancedParams':'▸ 高级参数','retry.advancedParamsHint':'默认值适合绝大多数场景；展开后才显示完整表单。','retry.saveButton':'保存重试链设置','settings.groupGeneralDesc':'决定调度器何时接管请求，以及账号之间如何排序。','settings.groupRefreshDesc':'控制插件多久向 Codex 查询一次各账号的额度，以及缓存何时算过期。','settings.groupRecoveryDesc':'账号出错或额度耗尽后的自动处理。⚠ 标记的选项有副作用，改动前请读说明。','settings.groupCircuitDesc':'账号连续失败后临时停用一段时间，避免持续向故障账号发请求。','settings.groupLogDesc':'插件日志与托管停用历史的容量上限，超出后丢弃最旧记录。',
    'app.title':'Codex 额度调度器','app.subtitle':'优化版 Fill First。配置、别名、分组、标签和备注由插件内部状态文件保存。','app.language':'界面语言','connection.managementKey':'CPA 管理密钥','connection.rememberManagementKey':'在此浏览器中记住管理密钥','connection.rememberManagementKeyHelp':'密钥将以未加密形式保存在浏览器本地存储中。请仅在受信任的设备上启用。','connection.backgroundHint':'只要调度器启动了，它就会在后台自动运行，无需保持页面开启。',
    'resetProbe.warningTitle':'自动激活新的额度周期默认关闭','resetProbe.warningBody':'开启后，调度器会在额度重置时间已到但新周期尚未生成时，发送一次极小的 Codex 请求尝试激活新周期。',
    'settings.title':'调度设置','settings.summary':'默认配置已经都设置好了，正常情况下不需要手动设置。','settings.handleEnabled':'启用调度接管','settings.usageFeedback':'失败反馈标记额度耗尽','settings.enableResetProbe':'自动激活新的额度周期','settings.enableResetProbeHelp':'即使普通刷新处于休眠状态，Probe 也会按额度刷新间隔执行只读检查，最短 30 分钟；只有检测到延迟启动的重置窗口时，才发送一次极小请求。可能消耗少量额度。','settings.provisionalProbe':'账号列表未确认时仍允许额度探测（高风险）','settings.provisionalProbeHelp':'CPA 暂时无法确认当前账号及优先级时，允许插件使用最近一次保存的账号列表执行额度重置探测。每次都会重新验证账号凭据，但仍无法保证账号未被删除或调整优先级。通常应保持关闭。','settings.monthlyMode':'月度账号使用方式','settings.acrossPriorities':'更高优先级耗尽时使用较低优先级账号','settings.acrossPrioritiesHelp':'开启后，所有 CPA 优先级层级的账号都会纳入调度：更高层级只要还有可用账号就一定优先，仅在更高层级全部不可用时才选用低层级。关闭则只调度最高优先级层。','settings.managedDisable':'额度耗尽时在 CPA 层停用账号并自动恢复','settings.managedDisableHelp':'收到确认的额度耗尽反馈时，把该账号的 CPA 凭据标记为 disabled；之后按刷新间隔只读检查额度，只有证实两个额度窗口都可用时才自动恢复启用。人工手动停用的账号绝不会被改动。默认关闭；开启后插件会写宿主的认证文件。','settings.expiryOrder':'按到期时间排序','settings.monthlyPriority':'优先使用月度账号','settings.refreshInterval':'额度刷新间隔','settings.staleAfter':'缓存过期判定','settings.refreshActiveWindow':'活跃刷新窗口','settings.refreshAfterResetDelay':'重置后刷新延迟','settings.refreshRetryDelays':'刷新失败重试间隔','settings.authFailureRetryInterval':'认证失败重试间隔','settings.refreshOnStartup':'启动时刷新额度','settings.maxConcurrency':'最大并发刷新','settings.circuitFailureThreshold':'熔断失败阈值','settings.circuitOpenDuration':'熔断等待时间','settings.circuitHalfOpenSuccessThreshold':'半开恢复成功次数','settings.maxLogEntries':'最大日志条数','settings.lifecycleEventLimit':'托管停用历史条数上限','settings.logRetention':'日志保留时间',
    'actions.loadData':'加载数据','actions.saveSettings':'保存设置','actions.refreshQuota':'刷新额度','actions.exportConfig':'导出配置','actions.importConfig':'导入配置','actions.refreshLogs':'刷新日志','actions.exportLogs':'导出日志','actions.close':'关闭','actions.saveAccount':'保存账号','actions.cancel':'取消','actions.pinAccount':'固定账号','actions.unpinAccount':'取消固定账号',
    'queue.title':'账号队列','queue.description':'账号卡片按当前调度优先级排序。第一个可用账号就是下一次 Codex 请求会优先选择的账号。','queue.aria':'账号卡片','metrics.nextAccount':'下一账号','metrics.monthlyMode':'Monthly','metrics.lastSelected':'最近选择',
    'logs.title':'调度日志','logs.empty':'暂无日志。发起请求或手动刷新额度后，这里会显示记录。',
    'edit.title':'编辑账号','edit.alias':'别名','account.schedulerPriority':'插件优先级','edit.groupID':'分组 ID','edit.groupName':'分组名称','edit.tags':'标签','edit.notes':'账号备注','edit.groupNotes':'分组备注',
    'notice.settingsSaved':'设置已保存，页面内容会自动更新。','notice.statusLoaded':'已加载当前设置。请确认后再次保存。','notice.refreshRequested':'已请求后台刷新额度，页面内容会自动更新。','notice.accountSaved':'账号卡片已保存，页面内容会自动更新。','notice.accountPinned':'账号已固定，插件优先级已设为 999。','notice.accountUnpinned':'已取消固定，插件优先级已重置为 0。','notice.refreshOneRequested':'已请求刷新该账号额度，页面内容会自动更新。','notice.configExported':'配置已导出。','notice.logsExported':'日志已导出。','notice.configImported':'配置已导入，页面内容会自动更新。','error.requestFailed':'请求失败：{status}','error.managementKeyRequired':'需要填写 CPA 管理密钥','error.schedulerPriorityInteger':'插件优先级必须是安全整数。','actions.pinAccountHelp':'把该账号固定到调度队列最前。','actions.unpinAccountHelp':'取消固定，恢复按额度调度。',
  }
};
TRANSLATIONS.en['retry.always']='Always retry all models';
TRANSLATIONS.en['retry.alwaysHelp']='Retry every model, including models without a configured chain. When no fallback is configured, retry the same model. Mutually exclusive with Shadow mode.';
TRANSLATIONS.en['retry.modes']='Shadow mode and Always retry cannot both be enabled.';
TRANSLATIONS.en['retry.checkCPA']='Check & fix CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmTitle']='Review CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmYes']='Apply recommended settings';
TRANSLATIONS.en['retry.cpaConfirmNo']='Cancel';
TRANSLATIONS.en['retry.cpaChecking']='Checking CPA retry settings...';
TRANSLATIONS.en['retry.cpaOK']='CPA retry settings are correct.';
TRANSLATIONS.en['retry.cpaFixed']='CPA retry settings were fixed and hot-reloaded.';
TRANSLATIONS.en['retry.cpaFailed']='Could not update CPA retry settings: {error}';
TRANSLATIONS.en['retry.cpaUpgrade']='CPA {version} is too old for this feature. Please upgrade to v7.3.4 or newer.';
TRANSLATIONS.en['retry.cpaVersionUnknown']='Could not determine the CPA version; upgrade CPA to v7.3.4 or newer before changing these settings.';
TRANSLATIONS.en['retry.cpaConfirmTitle']='Review CPA retry settings';
TRANSLATIONS.en['retry.cpaConfirmYes']='Apply recommended settings';
TRANSLATIONS.en['retry.cpaConfirmNo']='Cancel';
TRANSLATIONS.en['retry.cpaConfirmBody']='Current: request-retry={requestRetry}, buffering={buffering}, timeout={timeout}, bootstrap-retries={bootstrapRetries}. Recommended: request-retry=3, buffering=true, timeout="0", bootstrap-retries=1.';
TRANSLATIONS.en['retry.cpaNoChanges']='No changes made.';
TRANSLATIONS.en['log.retry.attempt_started']='Retry chain attempt started';
TRANSLATIONS.en['log.retry.retry_scheduled']='Retry scheduled after a retryable upstream failure';
TRANSLATIONS.en['log.retry.aborted']='Retry stopped because the upstream failure was not retryable';
TRANSLATIONS.en['log.retry.exhausted']='Retry chain exhausted; returning the last failure';
TRANSLATIONS.en['log.retry.shadow_would_retry']='Shadow mode: this request would have been retried';
TRANSLATIONS['zh-CN']['retry.always']='全局重试（所有模型）';
TRANSLATIONS['zh-CN']['retry.alwaysHelp']='对没有配置模型链的请求也启用重试；没有备用模型时按同一模型再次尝试。与影子模式互斥。';
TRANSLATIONS['zh-CN']['retry.modes']='影子模式和全局重试不能同时启用。';
TRANSLATIONS['zh-CN']['retry.checkCPA']='检查并修复 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmTitle']='确认修改 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmYes']='应用推荐设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmNo']='取消';
TRANSLATIONS['zh-CN']['retry.cpaChecking']='正在检查 CPA 重试设置…';
TRANSLATIONS['zh-CN']['retry.cpaOK']='CPA 重试设置正确。';
TRANSLATIONS['zh-CN']['retry.cpaFixed']='CPA 重试设置已修复并热加载。';
TRANSLATIONS['zh-CN']['retry.cpaFailed']='无法更新 CPA 重试设置：{error}';
TRANSLATIONS['zh-CN']['retry.cpaUpgrade']='当前 CPA {version} 太旧，无法使用此功能。请升级到 v7.3.4 或更高版本。';
TRANSLATIONS['zh-CN']['retry.cpaVersionUnknown']='无法确定 CPA 版本；请先将 CPA 升级到 v7.3.4 或更高版本，再修改这些设置。';
TRANSLATIONS['zh-CN']['retry.cpaConfirmTitle']='确认 CPA 重试设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmYes']='应用推荐设置';
TRANSLATIONS['zh-CN']['retry.cpaConfirmNo']='取消';
TRANSLATIONS['zh-CN']['retry.cpaConfirmBody']='当前值：request-retry={requestRetry}，buffering={buffering}，timeout={timeout}，bootstrap-retries={bootstrapRetries}。推荐值：request-retry=3，buffering=true，timeout="0"，bootstrap-retries=1。';
TRANSLATIONS['zh-CN']['retry.cpaNoChanges']='未做任何修改。';
TRANSLATIONS['zh-CN']['log.retry.attempt_started']='重试链开始尝试';
TRANSLATIONS['zh-CN']['log.retry.retry_scheduled']='上游失败可重试，已安排下一次尝试';
TRANSLATIONS['zh-CN']['log.retry.aborted']='上游失败不可重试，已停止重试';
TRANSLATIONS['zh-CN']['log.retry.exhausted']='重试链已用尽，返回最后一次失败';
TRANSLATIONS['zh-CN']['log.retry.shadow_would_retry']='影子模式：该请求本应重试';
const notice=document.getElementById('notice');
const editDialog=document.getElementById('editDialog');
const localeSelect=document.getElementById('localeSelect');
const settingsPanel=document.getElementById('settingsPanel');
const retryPanel=document.getElementById('retryPanel');
const retryChainEditor=document.getElementById('retryChainEditor');
const accountsByID=new Map();
function retryElement(id){return document.getElementById(id)||document.createElement('input')}
function retryTextValue(id,fallback){const node=document.getElementById(id);const raw=node?String(node.value||'').trim():'';return raw||fallback||''}
function retryNormalizeDuration(raw){const value=String(raw||'').trim();if(!value)return '';return /^\d+(\.\d+)?$/.test(value)?value+'s':value}
function retryDurationValue(id,fallback){const node=document.getElementById(id);const raw=node?retryNormalizeDuration(node.value):'';return raw||fallback||''}
function retryIntValue(id,fallback){const node=document.getElementById(id);const parsed=Number.parseInt(node?node.value:'',10);return Number.isFinite(parsed)&&parsed>0?parsed:fallback}
function collectRetryChainRows(){const rows=[];if(!retryChainEditor)return rows;for(const rowNode of retryChainEditor.querySelectorAll('[data-retry-row]')){const modelInput=rowNode.querySelector('[data-retry-model]');const model=modelInput?String(modelInput.value||'').trim():'';if(!model)continue;const fallbacks=[];for(const fallbackNode of rowNode.querySelectorAll('[data-retry-fallback]')){const fallbackModelInput=fallbackNode.querySelector('[data-retry-fallback-model]');const fallbackModel=fallbackModelInput?String(fallbackModelInput.value||'').trim():'';if(!fallbackModel)continue;fallbacks.push({provider:'',model:fallbackModel})}rows.push({model:model,fallbacks:fallbacks})}return rows}
function retryOptionNode(value){const option=document.createElement('option');option.value=value;return option}
function addRetryChainRow(){if(!retryChainEditor)return;retryClearEmptyState();retryChainEditor.append(retryChainRowNode({model:'',fallbacks:[{provider:'',model:''}]}))}
function renumberRetryFallbacks(container){if(!container)return;for(const [index,wrapper] of Array.from(container.querySelectorAll('[data-retry-fallback]')).entries()){const label=wrapper.querySelector('[data-retry-fallback-label]');if(label)label.textContent=index===0?t('retry.fallback'):t('retry.fallback')+' '+(index+1)}}
function retryFallbackNode(target,container,index){const wrapper=node('div','retryFallback');wrapper.dataset.retryFallback='1';const modelField=node('label','field');const fallbackLabel=node('span','',index>1?t('retry.fallback')+' '+index:t('retry.fallback'));fallbackLabel.dataset.retryFallbackLabel='1';modelField.append(fallbackLabel);const modelInput=document.createElement('input');modelInput.setAttribute('list','retryModelOptions');modelInput.setAttribute('spellcheck','false');modelInput.dataset.retryFallbackModel='1';modelInput.value=(target&&target.model)||'';modelField.append(modelInput);const remove=node('button','ghost retryRemove','');remove.type='button';remove.textContent=t('retry.remove');remove.addEventListener('click',()=>{const siblings=container?container.querySelectorAll('[data-retry-fallback]'):[];if(siblings.length<=1){modelInput.value=''}else{wrapper.remove();renumberRetryFallbacks(container)}});wrapper.append(modelField,remove);return wrapper}
function retryClearEmptyState(){if(!retryChainEditor)return;for(const empty of retryChainEditor.querySelectorAll('.retryEmpty'))empty.remove()}
function retryEnsureEmptyState(){if(!retryChainEditor)return;if(retryChainEditor.querySelector('[data-retry-row]'))return;if(retryChainEditor.querySelector('.retryEmpty'))return;retryChainEditor.append(node('div','retryEmpty',t('retry.chainEmpty')))}
function retryChainRowNode(row){const wrapper=node('div','retryChainRow');wrapper.dataset.retryRow='1';const head=node('div','retryChainRowHead');const modelField=node('label','field');modelField.append(node('span','',t('retry.rowModel')));const modelInput=document.createElement('input');modelInput.setAttribute('list','retryModelOptions');modelInput.setAttribute('spellcheck','false');modelInput.dataset.retryModel='1';modelInput.value=(row&&row.model)||'';modelField.append(modelInput);const actions=node('div','retryRowActions');const addFallback=node('button','ghost','');addFallback.type='button';addFallback.textContent=t('retry.addFallback');const removeRow=node('button','ghost','');removeRow.type='button';removeRow.textContent=t('retry.removeRow');actions.append(addFallback,removeRow);head.append(modelField,actions);const fallbacks=node('div','retryFallbacks');const source=row&&Array.isArray(row.fallbacks)&&row.fallbacks.length?row.fallbacks:[{provider:'',model:''}];source.forEach((target,index)=>fallbacks.append(retryFallbackNode(target,fallbacks,index+1)));addFallback.addEventListener('click',()=>{fallbacks.append(retryFallbackNode({provider:'',model:''},fallbacks,fallbacks.querySelectorAll('[data-retry-fallback]').length+1));renumberRetryFallbacks(fallbacks)});removeRow.addEventListener('click',()=>{wrapper.remove();retryEnsureEmptyState()});wrapper.append(head,fallbacks);return wrapper}
function renderRetryChainEditor(rows){if(!retryChainEditor)return;retryChainEditor.replaceChildren();for(const row of (Array.isArray(rows)?rows:[]))retryChainEditor.append(retryChainRowNode(row));retryEnsureEmptyState()}
function syncRetryModes(){const shadow=retryElement('retryShadow');const always=retryElement('retryAlways');if(shadow)shadow.disabled=always&&always.checked;if(always)always.disabled=shadow&&shadow.checked;syncRetryModeSegment()}
function syncRetryModeSegment(){const enabled=retryElement('retryEnabled').checked===true,shadow=retryElement('retryShadow').checked===true,always=retryElement('retryAlways').checked===true;const mode=!enabled?'off':shadow?'shadow':always?'always':'live';for(const radio of document.querySelectorAll('input[name="retryMode"]'))radio.checked=radio.value===mode}
function applyRetryMode(mode){retryElement('retryEnabled').checked=mode!=='off';retryElement('retryShadow').checked=mode==='shadow';retryElement('retryAlways').checked=mode==='always';syncRetryModes()}
for(const radio of document.querySelectorAll('input[name="retryMode"]'))radio.addEventListener('change',()=>{if(radio.checked)applyRetryMode(radio.value)})
function fillRetrySettings(){const s=STATUS.settings||{};retryElement('retryEnabled').checked=s.retry_enabled===true;retryElement('retryShadow').checked=s.retry_shadow===true;retryElement('retryAlways').checked=s.retry_always===true;syncRetryModes();retryElement('retryStripReasoning').checked=s.retry_strip_reasoning!==false;retryElement('retryMaxAttempts').value=s.retry_max_attempts||4;retryElement('retryMaxFrames').value=s.retry_max_frames||4096;retryElement('retryStallTimeout').value=s.retry_stall_timeout||'60s';retryElement('retryHoldTimeout').value=s.retry_hold_timeout||'90s';retryElement('retryChainDeadline').value=s.retry_chain_deadline||'240s';retryElement('retryMaxBytes').value=s.retry_max_bytes||'8MB';renderRetryChainEditor(s.retry_chain||[])}
const PIN_PRIORITY=999
const RETRY_DURATION_RE=/^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/i;
const RETRY_SIZE_RE=/^\d+(\.\d+)?(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$/i;
const RETRY_DURATION_FIELDS=[['retry_stall_timeout','retry.stallTimeout'],['retry_hold_timeout','retry.holdTimeout'],['retry_chain_deadline','retry.chainDeadline']];
function retryPayloadError(payload){const attempts=Number(payload.retry_max_attempts);if(!Number.isSafeInteger(attempts)||attempts<1||attempts>16)return {key:'retry.error.attempts',field:'retry.maxAttempts',value:payload.retry_max_attempts};const frames=Number(payload.retry_max_frames);if(!Number.isSafeInteger(frames)||frames<1)return {key:'retry.error.frames',field:'retry.maxFrames',value:payload.retry_max_frames};if(!RETRY_SIZE_RE.test(String(payload.retry_max_bytes||'')))return {key:'retry.error.size',field:'retry.maxBytes',value:payload.retry_max_bytes};for(const pair of RETRY_DURATION_FIELDS){if(!RETRY_DURATION_RE.test(String(payload[pair[0]]||'')))return {key:'retry.error.duration',field:pair[1],value:payload[pair[0]]}}if(payload.retry_shadow&&payload.retry_always)return {key:'retry.modes'};if(payload.retry_enabled&&!payload.retry_always){if(!Array.isArray(payload.retry_chain)||payload.retry_chain.length===0)return {key:'retry.invalidRow'};for(const row of payload.retry_chain){if(!row.model||!Array.isArray(row.fallbacks)||row.fallbacks.length===0)return {key:'retry.invalidRow'}}}return null}
function retryProblemMessage(problem){const label=problem.field?t(problem.field):'';const value=problem.value===undefined||problem.value===null?'':String(problem.value).trim();const detail=[label,value].filter(Boolean).join(': ');return detail?t(problem.key)+' ('+detail+')':t(problem.key)}
function collectRetryPayload(){const s=STATUS.settings||{};return{handle_enabled:s.handle_enabled!==false,enable_usage_feedback:s.enable_usage_feedback!==false,enable_reset_probe:s.enable_reset_probe===true,probe_on_provisional_roster:s.probe_on_provisional_roster===true,monthly_mode:s.monthly_mode||'expiry_order',schedule_across_priorities:s.schedule_across_priorities!==false,enable_managed_quota_disable:s.enable_managed_quota_disable===true,lifecycle_event_limit:s.lifecycle_event_limit||50,quota_refresh_interval:s.quota_refresh_interval||'30m0s',stale_after:s.stale_after||'5h0m0s',refresh_active_window:s.refresh_active_window||'1h0m0s',refresh_after_reset_delay:s.refresh_after_reset_delay||'1m0s',refresh_retry_delays:s.refresh_retry_delays||'1m0s,5m0s,15m0s',auth_failure_retry_interval:s.auth_failure_retry_interval||'30m0s',refresh_on_startup:s.refresh_on_startup===true,max_refresh_concurrency:s.max_refresh_concurrency||1,circuit_failure_threshold:s.circuit_failure_threshold||5,circuit_open_duration:s.circuit_open_duration||'30m0s',circuit_half_open_success_threshold:s.circuit_half_open_success_threshold||2,max_log_entries:s.max_log_entries||200,log_retention:s.log_retention||'24h0m0s',retry_enabled:retryElement('retryEnabled').checked===true,retry_shadow:retryElement('retryShadow').checked===true,retry_always:retryElement('retryAlways').checked===true,retry_strip_reasoning:retryElement('retryStripReasoning').checked!==false,retry_max_attempts:retryIntValue('retryMaxAttempts',4),retry_max_frames:retryIntValue('retryMaxFrames',4096),retry_stall_timeout:retryDurationValue('retryStallTimeout','60s'),retry_hold_timeout:retryDurationValue('retryHoldTimeout','90s'),retry_chain_deadline:retryDurationValue('retryChainDeadline','240s'),retry_max_bytes:retryTextValue('retryMaxBytes','8MB'),retry_chain:collectRetryChainRows()}}
async function saveRetrySettings(){try{if(!statusLoaded){await loadStatus();return}const payload=collectRetryPayload();const problem=retryPayloadError(payload);if(problem){showNotice(retryProblemMessage(problem),true,problem.key);return}await requestManagement('/settings',{method:'PUT',body:payload});settingsDirty=false;showNotice(t('notice.retrySaved'),false,'notice.retrySaved');await refreshStatus({management:true,fillSettings:true});await refreshRetryStats()}catch(error){showNotice(error.message||String(error),true)}}
async function requestManagementRoot(path,query){const headers=authHeaders();let url='/v0/management'+path;if(query){const params=new URLSearchParams(query);url+='?'+params.toString()}const resp=await fetch(url,{method:'GET',headers});const data=await readJSON(resp);if(!resp.ok){const message=data.error||data.message||t('error.requestFailed',{status:resp.status});throw new Error(message)}return data}
async function requestManagementText(path,options){const opts=options||{};const headers=authHeaders();if(opts.contentType)headers['Content-Type']=opts.contentType;const init={method:opts.method||'GET',headers};if(Object.prototype.hasOwnProperty.call(opts,'body'))init.body=String(opts.body);const resp=await fetch('/v0/management'+path,init);const body=await resp.text();if(!resp.ok){let message=body;try{const data=JSON.parse(body);message=data.error||data.message||body}catch(error){}throw new Error(message||t('error.requestFailed',{status:resp.status}))}return body}
function cpaRetrySettingsCorrect(config){const codex=config&&config.codex||{};const streaming=config&&config.streaming||{};return Number(config&&config['request-retry'])===3&&codex['stream-bootstrap-buffering']===true&&String(codex['stream-bootstrap-timeout']===undefined?'':codex['stream-bootstrap-timeout'])==='0'&&Number(streaming['bootstrap-retries'])===1}
function yamlTopLevelValue(yaml,key,value){const lines=String(yaml||'').split(/\r?\n/);const prefix=key+':';for(let index=0;index<lines.length;index+=1){const line=lines[index];if(/^\S/.test(line)&&line.trim().startsWith(prefix)){lines[index]=key+': '+value;return lines.join('\n')}}lines.unshift(key+': '+value);return lines.join('\n')}
function yamlNestedValue(yaml,section,key,value){const lines=String(yaml||'').split(/\r?\n/);let sectionIndex=-1;let sectionIndent=-1;for(let index=0;index<lines.length;index+=1){const line=lines[index];const trimmed=line.trim();if(trimmed===section+':'||trimmed.startsWith(section+': #')){sectionIndex=index;sectionIndent=line.search(/\S/);break}}if(sectionIndex<0){if(lines.length&&lines[lines.length-1]!=='')lines.push('');lines.push(section+':','  '+key+': '+value);return lines.join('\n')}let insertAt=sectionIndex+1;let childIndent=sectionIndent+2;for(let index=sectionIndex+1;index<lines.length;index+=1){const line=lines[index];const trimmed=line.trim();if(!trimmed||trimmed.startsWith('#')){insertAt=index+1;continue}const indent=line.search(/\S/);if(indent<=sectionIndent)break;insertAt=index+1;childIndent=indent;if(trimmed.startsWith(key+':')){lines[index]=' '.repeat(indent)+key+': '+value;return lines.join('\n')}}lines.splice(insertAt,0,' '.repeat(childIndent)+key+': '+value);return lines.join('\n')}
function cpaVersionAtLeast(version,major,minor,patch){const match=String(version||'').match(/v?(\d+)\.(\d+)\.(\d+)/i);if(!match)return false;const actual=[Number(match[1]),Number(match[2]),Number(match[3])];const required=[major,minor,patch];for(let index=0;index<3;index+=1){if(actual[index]!==required[index])return actual[index]>required[index]}return true}
async function getCPAVersion(){const resp=await fetch('/v0/management/config',{method:'GET',headers:authHeaders()});return resp.headers.get('X-CPA-VERSION')||resp.headers.get('X-SERVER-VERSION')||''}
function confirmCPASettingsInline(config){const panel=document.getElementById('retryCPAConfirm');const body=document.getElementById('retryCPAConfirmBody');if(!panel||!body)return Promise.resolve(false);const codex=config&&config.codex||{};const streaming=config&&config.streaming||{};const values=[['request-retry',config&&config['request-retry'],3],['stream-bootstrap-buffering',codex['stream-bootstrap-buffering'],true],['stream-bootstrap-timeout',codex['stream-bootstrap-timeout'],'0'],['bootstrap-retries',streaming['bootstrap-retries'],1]];body.replaceChildren();const table=document.createElement('table');const head=document.createElement('tr');for(const label of [currentLocale==='en'?'Setting':'设置',currentLocale==='en'?'Current':'当前值',currentLocale==='en'?'Recommended':'推荐值'])head.append(node('th','',label));table.append(head);for(const row of values){const tr=document.createElement('tr');const different=String(row[1])!==String(row[2]);tr.append(node('td',different?'diff':'',row[0]),node('td',different?'diff':'',String(row[1])),node('td',different?'diff':'',String(row[2])));table.append(tr)}body.append(table);panel.hidden=false;return new Promise((resolve)=>{const yes=document.getElementById('retryCPAConfirmYes');const no=document.getElementById('retryCPAConfirmNo');const finish=(accepted)=>{panel.hidden=true;yes.removeEventListener('click',onYes);no.removeEventListener('click',onNo);resolve(accepted)};const onYes=()=>finish(true);const onNo=()=>finish(false);yes.addEventListener('click',onYes);no.addEventListener('click',onNo)})}
async function checkAndFixCPASettings(){const button=document.getElementById('retryCheckCPA');const status=document.getElementById('retryCPAStatus');if(button)button.disabled=true;if(status)status.textContent=t('retry.cpaChecking');try{const version=await getCPAVersion();if(!version){if(status)status.textContent=t('retry.cpaVersionUnknown');return}if(!cpaVersionAtLeast(version,7,3,4)){if(status)status.textContent=t('retry.cpaUpgrade',{version:version});return}const before=await requestManagementRoot('/config');if(cpaRetrySettingsCorrect(before)){if(status)status.textContent=t('retry.cpaOK');return}if(!await confirmCPASettingsInline(before)){if(status)status.textContent=t('retry.cpaNoChanges');return}let yaml=await requestManagementText('/config.yaml');yaml=yamlTopLevelValue(yaml,'request-retry','3');yaml=yamlNestedValue(yaml,'codex','stream-bootstrap-buffering','true');yaml=yamlNestedValue(yaml,'codex','stream-bootstrap-timeout','"0"');yaml=yamlNestedValue(yaml,'streaming','bootstrap-retries','1');await requestManagementText('/config.yaml',{method:'PUT',contentType:'application/yaml',body:yaml});const after=await requestManagementRoot('/config');if(!cpaRetrySettingsCorrect(after))throw new Error('CPA did not report the requested settings after hot reload');if(status)status.textContent=t('retry.cpaFixed')}catch(error){if(status)status.textContent=t('retry.cpaFailed',{error:error.message||String(error)})}finally{if(button)button.disabled=false}}
function addRetryProvider(providers,name){const value=String(name||'').trim();if(value)providers.add(value)}
function collectRetryProviders(config,providers){providers.add('codex');providers.add('openai');if(!config||typeof config!=='object')return;const compat=config['openai-compatibility'];if(Array.isArray(compat)){for(const entry of compat){const name=String((entry&&entry.name)||'').trim();if(!name)continue;providers.add('openai-compatible-'+name.toLowerCase())}}for(const pair of [['gemini-api-key','gemini'],['claude-api-key','claude'],['vertex-api-key','vertex'],['codex-api-key','codex'],['interactions-api-key','interactions']]){if(config[pair[0]])providers.add(pair[1])}}
function retryModelKey(value){return String(value||'').trim().toLowerCase().replace(/[^a-z0-9]+/g,'')}
function addRetryModel(models,value){const id=String(value||'').trim();const key=retryModelKey(id);if(!key)return;const current=models.get(key);if(!current||((current.includes(' ')||current.includes('_'))&&!id.includes(' ')))models.set(key,id)}
function collectRetryModels(config,models){if(!config||typeof config!=='object')return;const compat=config['openai-compatibility'];if(!Array.isArray(compat))return;for(const entry of compat){const list=entry&&Array.isArray(entry.models)?entry.models:[];for(const model of list){for(const value of [model&&model.alias,model&&model.name])addRetryModel(models,value)}}}
function retrySetCatalogStatus(text){const node=document.getElementById('retryCatalogStatus');if(node)node.textContent=text}
function renderRetryCatalogOptions(){const providerList=document.getElementById('retryProviderOptions');const modelList=document.getElementById('retryModelOptions');if(providerList){providerList.replaceChildren();for(const value of retryCatalog.providers)providerList.append(retryOptionNode(value))}if(modelList){modelList.replaceChildren();for(const value of retryCatalog.models)modelList.append(retryOptionNode(value))}}
async function loadRetryCatalog(){retrySetCatalogStatus(t('retry.catalogLoading'));try{let config=null;try{config=await requestManagementRoot('/config')}catch(error){config=null}const providers=new Set();const models=new Map();collectRetryProviders(config,providers);collectRetryModels(config,models);let files=[];try{const listing=await requestManagementRoot('/auth-files');files=Array.isArray(listing.files)?listing.files:[]}catch(error){files=[]}let queried=0;for(const file of files){addRetryProvider(providers,file&&file.provider);const name=String((file&&file.name)||'').trim();if(!name||queried>=40)continue;queried+=1;try{const detail=await requestManagementRoot('/auth-files/models',{name:name});for(const model of (detail.models||[]))addRetryModel(models,model&&model.id)}catch(error){}}retryCatalog.providers=Array.from(providers).sort();retryCatalog.models=Array.from(models.values()).sort();retryCatalog.loaded=true;renderRetryCatalogOptions();retrySetCatalogStatus(retryCatalog.models.length===0?t('retry.catalogEmpty'):t('retry.catalogLoaded'))}catch(error){retrySetCatalogStatus(t('retry.catalogFailed'))}}
async function refreshRetryStats(){try{const data=await requestManagement('/retry/status',{query:{format:'json'}});retryStats=data||{};retryStatsLoaded=true}catch(error){retryStatsLoaded=false}renderRetryStats()}
function renderRetryStats(){const container=document.getElementById('retryStats');const events=document.getElementById('retryEvents');if(!container||!events)return;container.replaceChildren();const mode=String(retryStats.mode||'off');const modeKey=mode==='live'?'retry.modeLive':mode==='shadow'?'retry.modeShadow':'retry.modeOff';const cards=[[t('retry.mode'),t(modeKey)],[t('retry.requests'),retryStats.requests||0],[t('retry.retries'),retryStats.retries||0],[t('retry.succeeded'),retryStats.succeeded||0],[t('retry.failed'),retryStats.failed||0],[t('retry.shadowHits'),retryStats.shadow_hits||0]];for(const card of cards){const box=node('div','retryStat');box.append(node('span','retryStatLabel',card[0]),node('span','retryStatValue',String(card[1])));container.append(box)}const chain=Array.isArray(retryStats.chain)?retryStats.chain:[];if(chain.length){const summary=node('div','retryChainSummary');summary.append(node('div','retryChainSummaryTitle',t('retry.chainTitle')));for(const line of chain)summary.append(node('div','retryChainLine',line));container.append(summary)}events.replaceChildren();const history=Array.isArray(retryStats.events)?retryStats.events.slice().reverse():[];if(history.length===0){events.append(node('div','empty',t('retry.noEvents')));return}for(const event of history){const row=node('div','logItem info');const meta=node('div','logMeta',[event.at?formatLogTime(event.at):'',event.model||'',event.target||''].filter(Boolean).join(' · '));const detail=[event.outcome||'',event.kind||'',event.reason||'',event.next_target?'-> '+event.next_target:''].filter(Boolean).join(' ');row.append(meta,node('div','logMsg',detail));events.append(row)}}
const groupsByID=new Map();
let editingAuthID='';
let currentLocale=detectLocale();
let statusLoaded=!STATUS.shell;
let settingsDirty=false;
let settingsInitialized=!STATUS.shell;
let retryCatalog={providers:[],models:[],loaded:false};
let retryStats={};
let retryStatsLoaded=false;
let statusPollID=0;
const INLINE_TRANSLATIONS=[
  ['下一优先','Next preferred'],['可用','Available'],['未知类型','unknown type'],['CPA 优先级','CPA priority'],['插件优先级','Plugin priority'],['熔断：','Circuit: '],['熔断','Circuit'],['全开','closed'],['半开','half-open'],
  ['按到期时间','by expiry time'],['优先使用','prefer Monthly'],['已启用','enabled'],['已关闭','disabled'],['暂无数据','No data'],['暂无','None'],['已用完','Used up'],
  ['5 小时额度','5-hour quota'],['周额度','Weekly quota'],['月额度','Monthly quota'],['剩余 ','Remaining '],['5 小时重置：','5-hour reset: '],['长额度重置：','Long quota reset: '],
  ['缓存时间','Cache age'],['额度来源','Quota source'],['响应观测','Response observation'],['轮询刷新','Polled refresh'],['托管停用','Managed disable'],['已停用，恢复中','disabled, recovering'],['检查额度中','checking quota'],['熔断计数','Circuit counts'],['失败','Failures'],['成功','Successes'],['主动重置','Reset credits'],['调度状态','Scheduler state'],[' 次',' times'],['累计','total'],['有效期','expires'],
  ['刷新额度','Refresh Quota'],['编辑','Edit'],['请重新登录','Please re-login'],['不可用','Unavailable'],['最后错误','Last error'],['下次重试','Next retry'],['长额度','Long quota'],[' 重置：',' reset: '],['Monthly：','Monthly: '],['调度：','Scheduler: '],['下一账号：','Next account: '],['最近选择：','Last selected: '],
  ['最近一次扫描认证文件中没有 Codex 账号，请先进行 OS 登录，然后发送一次 Codex 请求或手动刷新额度。','The most recent auth-file scan found no Codex accounts. Please sign in through OS login, then send a Codex request or refresh quota manually.'],
  [' 内没有观察到 Codex 请求，系统暂不主动扫描账号。','; the system will not actively scan accounts. '],['发送第一次 Codex 请求后将自动获取账号额度信息。','Send the first Codex request to fetch account quota automatically.'],
  [' 内没有 Codex 请求，调度器已暂停后台刷新。',' without a Codex request, so background refresh is paused. '],['发送一次 Codex 请求后会重新进入活跃窗口并获取账号额度信息。','Send one Codex request to re-enter the active window and fetch account quota.'],
  ['已观察到 Codex 请求，调度器处于活跃窗口。','A Codex request was observed and the scheduler is in its active window. '],['账号额度刷新完成后，这里会显示账号卡片。','Account cards will appear here after quota refresh finishes.'],['等待额度刷新后，这里会显示账号卡片。','Account cards will appear here after quota refresh.'],
  ['未发现 Codex 账号','No Codex accounts were found'],['最近一次扫描认证文件中没有 Codex 账号','The most recent auth-file scan found no Codex accounts'],['请先进行 OS 登录','please sign in through OS login'],['然后发送一次 Codex 请求或手动刷新额度','then send a Codex request or refresh quota manually'],
  ['账号额度已过期或待刷新，但最近 ','Quota data is stale or pending refresh, but no Codex requests were seen in the last '],[' 内没有 Codex 请求，调度器处于休眠状态。','; the scheduler is sleeping. '],['发送一次 Codex 请求后会获取账号额度信息。','Send one Codex request to fetch account quota.'],
  ['调度器处于休眠状态','The scheduler is sleeping'],['最近 ','No Codex requests were observed in the last '],[' 内没有观察到 Codex 请求',''],['系统暂不主动扫描账号','the system will not actively scan accounts'],['发送第一次 Codex 请求后将自动获取账号额度信息','Send the first Codex request to fetch account quota automatically'],
  ['内没有 Codex 请求，调度器已暂停后台刷新',' without a Codex request, so background refresh is paused'],['发送一次 Codex 请求后会重新进入活跃窗口并获取账号额度信息','Send one Codex request to re-enter the active window and fetch account quota'],
  ['等待账号额度数据','Waiting for account quota data'],['已观察到 Codex 请求，调度器处于活跃窗口','A Codex request was observed and the scheduler is in its active window'],['账号额度刷新完成后，这里会显示账号卡片','Account cards will appear here after quota refresh finishes'],['等待额度刷新后，这里会显示账号卡片','Account cards will appear here after quota refresh'],
  ['认证信息异常，请重新登录。','Authentication looks invalid. Please re-login.'],['上次额度刷新失败，调度器正在等待下次自动重试。','The last quota refresh failed. The scheduler is waiting for the next automatic retry.'],
  ['账号额度已过期，调度器处于活跃窗口，将按刷新队列更新。','Quota data is stale. The scheduler is active and will update it through the refresh queue.'],['账号尚未获取额度信息，调度器处于活跃窗口，将按刷新队列更新。','Quota has not been fetched yet. The scheduler is active and will update it through the refresh queue.'],
  ['上次额度刷新失败，当前已到重试时间。','The last quota refresh failed, and retry is due now.'],['额度重置时间已到，调度器将按刷新队列更新。','Quota reset time has arrived; the scheduler will update it through the refresh queue.'],
  ['账号处于熔断等待中，半开探测时间到达后会重试。','The account is in circuit wait and will retry after the half-open probe time.'],['本地认证信息缺失或格式异常，请检查登录状态。','Local auth data is missing or malformed. Please check sign-in state.'],
  ['填写 CPA 管理密钥后将动态加载账号队列、调度日志和当前调度状态','Enter the CPA management key to dynamically load the account queue, scheduler logs, and current scheduler state'],['发送 Codex 请求后，调度器会在活跃窗口内刷新额度','After a Codex request, the scheduler refreshes quota within the active window']
];
const SORTED_INLINE_TRANSLATIONS=[...INLINE_TRANSLATIONS].sort((left,right)=>right[0].length-left[0].length);
const DUE_REASON_LABELS={
  en:{stale:'Quota data is stale or pending refresh',never_refreshed:'Never refreshed',retry_due:'Retry is due now',retry_wait:'Waiting before retry',refresh_interval_due:'Refresh interval due',five_hour_reset_due:'5-hour reset is due',long_window_reset_due:'Long quota reset is due',temporary_reset_due:'Temporary reset is due',circuit_wait:'Circuit wait',circuit_probe_due:'Circuit probe due',auth_failure:'Authentication needs re-login',local_failure:'Local auth data needs attention'},
  'zh-CN':{stale:'额度缓存过期',never_refreshed:'尚未刷新',retry_due:'重试时间已到',retry_wait:'等待重试',refresh_interval_due:'刷新间隔已到',five_hour_reset_due:'5 小时重置已到',long_window_reset_due:'长额度重置已到',temporary_reset_due:'主动重置已到',circuit_wait:'熔断等待',circuit_probe_due:'熔断探测已到',auth_failure:'认证异常',local_failure:'本地认证异常'}
};
const UNAVAILABLE_REASON_LABELS={
  en:{auth_failure:'Authentication needs re-login',local_failure:'Local auth data needs attention',stale_quota:'Quota data is stale or pending refresh',circuit_open:'Circuit open',quota_probe_wait:'Quota probe wait',temporary_exhausted:'Temporarily exhausted',five_hour_exhausted:'5-hour quota exhausted',weekly_exhausted:'Weekly quota exhausted',monthly_exhausted:'Monthly quota exhausted',unknown_account:'Unknown account'},
  'zh-CN':{auth_failure:'认证异常',local_failure:'本地认证异常',stale_quota:'额度缓存过期',circuit_open:'熔断中',quota_probe_wait:'等待额度探测',temporary_exhausted:'临时额度耗尽',five_hour_exhausted:'5 小时额度耗尽',weekly_exhausted:'周额度耗尽',monthly_exhausted:'月额度耗尽',unknown_account:'未知账号'}
};
function normalizeLocale(raw){return String(raw||'').toLowerCase().startsWith('zh')?'zh-CN':'en'}
function storedLocale(){try{const locale=window.localStorage.getItem(LOCALE_STORAGE_KEY);return locale==='zh-CN'||locale==='en'?locale:''}catch(error){return''}}
function parentLocale(){try{if(window.parent===window)return'';const language=String(window.parent.document.documentElement.lang||'').trim().toLowerCase();if(!language)return'';return language.startsWith('zh')?'zh-CN':'en'}catch(error){return''}}
function detectLocale(){return storedLocale()||parentLocale()||'en'}
function watchParentLocale(){try{if(window.parent===window)return;const root=window.parent.document.documentElement;new MutationObserver(()=>{if(storedLocale())return;const next=parentLocale();if(next&&next!==currentLocale){currentLocale=next;applyLocale()}}).observe(root,{attributes:true,attributeFilter:['lang']})}catch(error){}}
function t(key,params){const dictionary=TRANSLATIONS[currentLocale]||TRANSLATIONS.en;let message=dictionary[key]||TRANSLATIONS.en[key]||key;for(const name of Object.keys(params||{})){message=message.split('{'+name+'}').join(String(params[name]))}return message}
function labelFrom(table,key,fallback){const dictionary=table[currentLocale]||table.en;return dictionary[key]||fallback||key||''}
function labelDueReason(reason){return labelFrom(DUE_REASON_LABELS,reason,reason)}
function labelUnavailableReason(reason){return labelFrom(UNAVAILABLE_REASON_LABELS,reason,reason||'不可用')}
function translateInlineText(raw){let text=String(raw||'');if(currentLocale!=='en')return text;for(const pair of SORTED_INLINE_TRANSLATIONS){text=text.split(pair[0]).join(pair[1])}return text}
function applyInlineTranslations(){const nodes=document.querySelectorAll('.badge,.quota-title,.quota-value,.quota-reset,.metaFlow span,.kv span,.cardActions button,.pinBtn,.empty,.empty strong,.empty div');for(const node of nodes){if(node.children.length>0)continue;if(!node.dataset.rawText)node.dataset.rawText=node.textContent;node.textContent=translateInlineText(node.dataset.rawText)}formatLocalTimes()}
function applyLocale(){document.documentElement.lang=currentLocale;document.title=t('app.title');localeSelect.value=currentLocale;for(const node of document.querySelectorAll('[data-i18n]')){node.textContent=t(node.dataset.i18n)}for(const node of document.querySelectorAll('[data-i18n-aria]')){node.setAttribute('aria-label',t(node.dataset.i18nAria))}for(const node of document.querySelectorAll('.localeColon'))node.textContent=currentLocale==='en'?': ':'：';if(notice.dataset.i18nKey)notice.textContent=t(notice.dataset.i18nKey);for(const container of document.querySelectorAll('.retryFallbacks'))renumberRetryFallbacks(container);renderMetrics();renderAccounts(STATUS.accounts||[]);applyInlineTranslations();renderLogs(STATUS.logs||[])}
function changeLocale(locale){currentLocale=normalizeLocale(locale);try{window.localStorage.setItem(LOCALE_STORAGE_KEY,currentLocale)}catch(error){}applyLocale()}
let restoredManagementKey=false;
function syncManagementKeyVisibility(){const field=document.getElementById('managementKeyField');const remember=document.getElementById('rememberManagementKey');if(field&&remember)field.hidden=remember.checked&&restoredManagementKey}
function showManagementKeyInput(){restoredManagementKey=false;const field=document.getElementById('managementKeyField');const input=document.getElementById('managementKey');if(field)field.hidden=false;if(input){input.focus();input.select()}}
function restoreRememberedManagementKey(){try{const key=window.localStorage.getItem(MANAGEMENT_KEY_STORAGE_KEY);if(!key)return false;document.getElementById('managementKey').value=key;document.getElementById('rememberManagementKey').checked=true;restoredManagementKey=true;syncManagementKeyVisibility();return true}catch(error){return false}}
function syncRememberedManagementKey(){const remember=document.getElementById('rememberManagementKey').checked;const key=document.getElementById('managementKey').value.trim();syncManagementKeyVisibility();try{if(remember&&key)window.localStorage.setItem(MANAGEMENT_KEY_STORAGE_KEY,key);else window.localStorage.removeItem(MANAGEMENT_KEY_STORAGE_KEY)}catch(error){}}
let noticeTimer=null
function showNotice(text,isError,i18nKey){notice.hidden=false;notice.textContent=text;notice.className='notice'+(isError?' error':'');notice.dataset.i18nKey=i18nKey||'';if(noticeTimer)clearTimeout(noticeTimer);noticeTimer=setTimeout(()=>{notice.classList.add('fading');setTimeout(()=>{notice.hidden=true;notice.classList.remove('fading')},380)},isError?8000:4500)}
function rebuildDerivedState(){accountsByID.clear();groupsByID.clear();for(const account of STATUS.accounts||[]){if(account.auth_id)accountsByID.set(account.auth_id,account);if(account.group_id)groupsByID.set(account.group_id,{name:account.group||'',notes:account.group_notes||''})}for(const group of STATUS.groups||[]){if(group.id)groupsByID.set(group.id,{name:group.name||'',notes:group.notes||''})}}
function renderMetrics(){const empty=currentLocale==='en'?'None':'暂无';const monthlyMode=STATUS.monthly_mode==='priority'?(currentLocale==='en'?'prefer Monthly':'优先使用'):(currentLocale==='en'?'by expiry time':'按到期时间');const setText=(id,text)=>{const node=document.getElementById(id);if(node)node.textContent=text};setText('metricNextAuthID',STATUS.next_auth_id||empty);setText('metricMonthlyMode',monthlyMode);setText('metricLastSelected',STATUS.last_selected||empty)}
function hasManagementKey(){const input=document.getElementById('managementKey');return !!(input&&(input.value||'').trim())}
function settingsFocusedOrDirty(){const panels=[document.getElementById('settingsPanel'),document.getElementById('retryPanel')];return settingsDirty||panels.some((panel)=>panel&&panel.contains(document.activeElement))}
function updateResetProbeWarning(){const warning=document.getElementById('resetProbeWarning');if(warning)warning.hidden=!(statusLoaded&&STATUS.settings&&STATUS.settings.enable_reset_probe!==true)}
function updateProtectedVisibility(){const loaded=statusLoaded;const show=(id,visible)=>{const item=document.getElementById(id);if(item)item.hidden=!visible};show('settingsPanel',loaded);show('retryPanelWrap',loaded);show('protectedMain',loaded);show('refreshQuota',loaded);show('loadData',!loaded);updateResetProbeWarning()}
function renderRosterLifecycle(){const warning=document.getElementById('rosterLifecycleWarning');const title=document.getElementById('rosterLifecycleTitle');const body=document.getElementById('rosterLifecycleBody');if(!warning||!title||!body)return;const roster=STATUS.roster||{};const messages=[roster.warning,roster.risk_warning].filter(Boolean);warning.hidden=messages.length===0;title.textContent=[roster.capability,roster.health].filter(Boolean).join(' / ')||'Roster lifecycle';body.textContent=messages.join(' ')}
function applyStatus(data,options){STATUS=data;statusLoaded=true;rebuildDerivedState();const shouldFillSettings=(options&&options.fillSettings)||!settingsInitialized||!settingsFocusedOrDirty();if(shouldFillSettings){fillSettings();settingsInitialized=true}renderMetrics();renderRosterLifecycle();renderAccounts(STATUS.accounts||[]);renderLogs(STATUS.logs||[]);updateProtectedVisibility();applyLocale();if(statusLoaded)refreshRetryStats()}
async function refreshStatus(options){const opts=options||{};const data=await requestManagement('/status',{query:{format:'json'}});applyStatus(data,opts)}
function startStatusPolling(){if(statusPollID)return;statusPollID=window.setInterval(()=>refreshStatus({management:true}).catch(()=>{}),15000)}
async function loadStatus(){try{await refreshStatus({management:true,fillSettings:true});const remember=document.getElementById('rememberManagementKey');if(remember&&remember.checked&&hasManagementKey()){restoredManagementKey=true;syncManagementKeyVisibility()}showNotice(t('notice.statusLoaded'),false,'notice.statusLoaded');startStatusPolling()}catch(error){showNotice(error.message||String(error),true)}}
async function pollStatus(times,delayMs){for(let i=0;i<times;i++){await new Promise((resolve)=>window.setTimeout(resolve,delayMs));await refreshStatus()}}
function parseGoDurationSeconds(text){const raw=String(text||'').trim();if(!raw)return null;let total=0,seen=false;const re=/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gi;let part;while((part=re.exec(raw))){seen=true;const value=parseFloat(part[1]);const unit=part[2].toLowerCase();const mult={ns:1e-9,us:1e-6,'µs':1e-6,ms:1e-3,s:1,m:60,h:3600}[unit];if(mult===undefined)return null;total+=value*mult}return seen?total:null}
function durationUnitFor(seconds){if(seconds>=172800&&seconds%86400===0)return 'd';if(seconds%3600===0)return 'h';return 'm'}
function applyDurationSplit(){for(const input of document.querySelectorAll('input[data-duration-field]')){const select=document.querySelector('select[data-duration-unit="'+input.id+'"]');if(!select)continue;const seconds=parseGoDurationSeconds(input.value);if(seconds===null){select.value='raw';continue}const unit=durationUnitFor(seconds);const factor=unit==='d'?86400:unit==='h'?3600:60;const value=seconds/factor;input.value=String(Number.isInteger(value)?value:+value.toFixed(2));select.value=unit}}
function collectDurationInputs(){for(const input of document.querySelectorAll('input[data-duration-field]')){const select=document.querySelector('select[data-duration-unit="'+input.id+'"]');if(!select||select.value==='raw')continue;const value=parseFloat(input.value);if(Number.isFinite(value)&&value>0)input.value=String(value)+select.value}}
function collectSettingsPayload(){collectDurationInputs();const payload={handle_enabled:document.getElementById('handleEnabled').checked,enable_usage_feedback:document.getElementById('usageFeedback').checked,enable_reset_probe:document.getElementById('enableResetProbe').checked,probe_on_provisional_roster:document.getElementById('probeOnProvisionalRoster').checked,monthly_mode:document.getElementById('monthlyMode').value,schedule_across_priorities:document.getElementById('scheduleAcrossPriorities').checked,enable_managed_quota_disable:document.getElementById('enableManagedQuotaDisable').checked,lifecycle_event_limit:Number.parseInt(document.getElementById('lifecycleEventLimit').value,10)||50,quota_refresh_interval:document.getElementById('refreshInterval').value.trim(),stale_after:document.getElementById('staleAfter').value.trim(),refresh_active_window:document.getElementById('refreshActiveWindow').value.trim(),refresh_after_reset_delay:document.getElementById('refreshAfterResetDelay').value.trim(),refresh_retry_delays:document.getElementById('refreshRetryDelays').value.trim(),auth_failure_retry_interval:document.getElementById('authFailureRetryInterval').value.trim(),refresh_on_startup:document.getElementById('refreshOnStartup').checked,max_refresh_concurrency:Number.parseInt(document.getElementById('maxConcurrency').value,10)||1,circuit_failure_threshold:Number.parseInt(document.getElementById('circuitFailureThreshold').value,10)||5,circuit_open_duration:document.getElementById('circuitOpenDuration').value.trim(),circuit_half_open_success_threshold:Number.parseInt(document.getElementById('circuitHalfOpenSuccessThreshold').value,10)||2,max_log_entries:Number.parseInt(document.getElementById('maxLogEntries').value,10)||200,log_retention:document.getElementById('logRetention').value.trim(),retry_enabled:retryElement('retryEnabled').checked===true,retry_shadow:retryElement('retryShadow').checked===true,retry_always:retryElement('retryAlways').checked===true,retry_strip_reasoning:retryElement('retryStripReasoning').checked!==false,retry_max_attempts:retryIntValue('retryMaxAttempts',4),retry_max_frames:retryIntValue('retryMaxFrames',4096),retry_stall_timeout:retryDurationValue('retryStallTimeout','60s'),retry_hold_timeout:retryDurationValue('retryHoldTimeout','90s'),retry_chain_deadline:retryDurationValue('retryChainDeadline','240s'),retry_max_bytes:retryTextValue('retryMaxBytes','8MB'),retry_chain:collectRetryChainRows()};applyDurationSplit();return payload}
function node(tag,className,text){const item=document.createElement(tag);if(className)item.className=className;if(text!==undefined)item.textContent=text;return item}
function addKV(parent,key,value){parent.append(node('span','',key),node('span','',value||'暂无'))}
function metaFlowAdd(parent,key,value){if(value===''||value===null||value===undefined)return;parent.append(node('span','',key+'：'+value))}
function recoveryTextFrom(date){if(Number.isNaN(date.getTime())||date.getTime()-Date.now()<=0)return'';const text=resetRemainingText(date);return currentLocale==='en'?text+' to recovery':'恢复还需 '+text}
function quotaSourceLabel(source){if(source==='observed')return currentLocale==='en'?'Response observation':'响应观测';if(source==='polled')return currentLocale==='en'?'Polled refresh':'轮询刷新';return ''}
function managedDisableLabel(summary){const states={planned:currentLocale==='en'?'planned':'已计划',disabled:currentLocale==='en'?'disabled, recovering':'已停用，恢复中',checking:currentLocale==='en'?'checking quota':'检查额度中'};let text=states[summary.state]||summary.state;if(summary.next_check_at){const date=new Date(summary.next_check_at);if(!Number.isNaN(date.getTime()))text+='（'+date.toLocaleString(dateLocale(),{hour12:false})+'）'}return text}
function addBadge(text,className){return node('span','badge '+(className||''),text)}
function addQuota(windowData,label){const row=node('div','quota-row');const head=node('div','quota-head');const value=node('span','quota-value '+(quotaFillClass(windowData)||''));const pct=Math.max(0,Math.min(100,Math.round(Number(windowData.remaining_percent||0))));value.append(node('b','',pct+'%'));if(!windowData.missing&&windowData.reset_text){const date=new Date(windowData.reset_text);value.append(node('span','sep','·'));const resetSpan=node('span','localTime quotaResetTime',resetRemainingText(date));resetSpan.dataset.time=windowData.reset_text;resetSpan.className='quotaResetTime';value.append(resetSpan);row.title=(label||windowData.label||'')+' '+(Number.isNaN(date.getTime())?windowData.reset_text:date.toLocaleString(dateLocale(),{hour12:false}))}head.append(node('span','quota-title',label||windowData.label||''),value);const bar=node('div','quota-bar');const fill=node('div','quota-fill '+quotaFillClass(windowData));fill.style.width=pct+'%';bar.append(fill);row.append(head,bar);return row}
function quotaFillClass(windowData){if(windowData.missing)return'';const remaining=Number(windowData.remaining_percent||0);return windowData.exhausted||remaining<30?'danger':remaining<60?'warn':''}
function dateLocale(){return currentLocale==='en'?'en-US':'zh-CN'}
function resetCreditCount(value){if(currentLocale!=='en')return value+' 次';return value+' '+(Number(value)===1?'time':'times')}
function resetCreditSummary(account){const parts=[];if(account.reset_credits_available_count==null){parts.push(currentLocale==='en'?'None':'暂无')}else{parts.push(resetCreditCount(account.reset_credits_available_count))}if(account.reset_credits_total_earned_count!=null)parts.push(currentLocale==='en'?'/ total '+resetCreditCount(account.reset_credits_total_earned_count):'/ 累计 '+resetCreditCount(account.reset_credits_total_earned_count));for(const credit of account.reset_credits||[]){const raw=credit.expires_at||'';if(!raw)continue;const date=new Date(raw);const expiry=Number.isNaN(date.getTime())?raw:date.toLocaleString(dateLocale(),{hour12:false});const prefix=currentLocale==='en'?'; ':'；';const expires=currentLocale==='en'?'expires ':'有效期 ';parts.push(prefix+(credit.status?credit.status+' ':'')+expires+expiry)}return parts.join(' ')}
function renderEmptyState(){const empty=STATUS.empty_state||{};const title=empty.title||'暂无账号数据';const message=empty.message||'等待额度刷新后，这里会显示账号卡片。';const box=node('div','empty','');box.append(node('strong','',title),node('div','',message));return box}
function createPinButton(account){const pinned=account.scheduler_priority===PIN_PRIORITY;const button=node('button','pinBtn'+(pinned?' pinned':''));button.type='button';button.className='pinBtn pinAccount '+(pinned?'pinned':'');button.dataset.authId=account.auth_id||'';button.dataset.pinned=String(pinned);button.setAttribute('aria-pressed',String(pinned));const label=(pinned?'📌 ':'📌 ')+t(pinned?'actions.unpinAccount':'actions.pinAccount');button.textContent=label;button.title=t(pinned?'actions.unpinAccountHelp':'actions.pinAccountHelp');button.addEventListener('click',()=>togglePin(account.auth_id||'',pinned,button));return button}
async function togglePin(authID,pinned,button){if(!authID)return;if(!hasManagementKey()){showNotice(t('error.managementKeyRequired'),true,'error.managementKeyRequired');return}if(button)button.disabled=true;const priority=pinned?0:PIN_PRIORITY;const noticeKey=pinned?'notice.accountUnpinned':'notice.accountPinned';try{await requestManagement('/annotations/account',{method:'PATCH',body:{auth_id:authID,scheduler_priority:priority}});showNotice(t(noticeKey),false,noticeKey);await refreshStatus()}catch(error){showNotice(error.message||String(error),true);if(button)button.disabled=false}}
function renderAccounts(accounts){const queue=document.querySelector('section.queue');if(!queue)return;queue.replaceChildren();const items=Array.isArray(accounts)?accounts:[];if(items.length===0){queue.append(renderEmptyState());return}for(const account of items){const card=node('article','card '+(STATUS.next_auth_id&&STATUS.next_auth_id===account.auth_id?'next':''));card.dataset.authId=account.auth_id||'';const top=node('div','cardTop');const identity=node('div','identity');const titleLine=node('div','titleLine');titleLine.append(node('span','title',account.alias||account.auth_id||''));if(account.group)titleLine.append(node('span','groupPill',account.group));identity.append(titleLine);const sub=node('div','sub');const code=document.createElement('code');code.textContent=account.auth_id||'';sub.append(code);identity.append(sub);if(account.tags&&account.tags.length){const tags=node('div','metaLine');for(const tag of account.tags)tags.append(node('span','chip',tag));identity.append(tags)}const priorityLine=node('div','priorityLine');priorityLine.append('#'+(account.rank||'')+' · CPA '+(account.cpa_priority||0)+' · '+((currentLocale==='en')?'plugin ':'插件')+(((account.scheduler_priority===PIN_PRIORITY)?('📌 '+PIN_PRIORITY):(account.scheduler_priority||0))));top.append(identity,priorityLine);card.append(top);const badges=node('div','badges');if(STATUS.next_auth_id&&STATUS.next_auth_id===account.auth_id)badges.append(addBadge('下一优先','next'));if(account.available){badges.append(addBadge('可用','ok'))}else{const reason=addBadge(labelUnavailableReason(account.unavailable_reason),'no');const recovery=account.next_retry_text||(account.five_hour&&!account.five_hour.missing&&account.five_hour.reset_text?recoveryTextFrom(new Date(account.five_hour.reset_text)):'');if(recovery)reason.textContent+=' · '+recovery;badges.append(reason)}badges.append(addBadge(account.family||'未知类型'),addBadge('熔断：'+((account.circuit&&account.circuit.label)||'')));if(account.auth_failure)badges.append(addBadge('请重新登录','no'));if(account.refresh_due_reason)badges.append(addBadge(labelDueReason(account.refresh_due_reason)));card.append(badges);const quotaList=node('div','quotaList');if(account.five_hour&&!account.five_hour.missing)quotaList.append(addQuota(account.five_hour,'5 小时额度'));if(account.long_window)quotaList.append(addQuota(account.long_window,account.long_window.label||'长额度'));card.append(quotaList);const meta=node('div','metaFlow');metaFlowAdd(meta,'缓存时间',account.cache_age||'暂无');metaFlowAdd(meta,'额度来源',quotaSourceLabel(account.quota_source));metaFlowAdd(meta,'主动重置',resetCreditSummary(account));if(account.managed_disable)metaFlowAdd(meta,'托管停用',managedDisableLabel(account.managed_disable));if(account.status_note)metaFlowAdd(meta,'调度状态',account.status_note);if(account.last_error)metaFlowAdd(meta,'最后错误',account.last_error);if(account.next_retry_text&&account.available)metaFlowAdd(meta,'下次重试',account.next_retry_text);card.append(meta);if(account.notes||account.group_notes){const notes=node('div','noteBlock');if(account.notes)notes.append(node('div','',account.notes));if(account.group_notes)notes.append(node('div','',account.group_notes));card.append(notes)}const actions=node('div','cardActions');const refresh=node('button','ghost refreshOne','刷新额度');refresh.type='button';refresh.addEventListener('click',()=>refreshOneQuota(account.auth_id||''));const edit=node('button','secondary openEdit','编辑');edit.type='button';edit.addEventListener('click',()=>openEdit(account.auth_id||''));actions.append(createPinButton(account),refresh,edit);card.append(actions);queue.append(card)}formatLocalTimes()}

async function readJSON(resp){const text=await resp.text();if(!text)return{};try{return JSON.parse(text)}catch{return{error:text}}}
function authHeaders(){const input=document.getElementById('managementKey');const key=(input&&input.value||'').trim();if(!key)throw new Error(t('error.managementKeyRequired'));const name='Author'+'ization';const scheme='Bea'+'rer ';const headers={};headers[name]=key.toLowerCase().startsWith(scheme.toLowerCase())?key:scheme+key;return headers}
async function requestManagement(path,options){const opts=options||{};const headers=authHeaders();let url=MANAGEMENT_BASE+path;if(opts.query){const params=new URLSearchParams(opts.query);url+='?'+params.toString()}const init={method:opts.method||'GET',headers};if(Object.prototype.hasOwnProperty.call(opts,'body')){headers['Content-Type']=opts.contentType||'application/json';init.body=typeof opts.body==='string'?opts.body:JSON.stringify(opts.body)}const resp=await fetch(url,init);const data=await readJSON(resp);const message=data.error||data.message||t('error.requestFailed',{status:resp.status});if(!resp.ok){if(resp.status===401||/invalid management key/i.test(message))showManagementKeyInput();throw new Error(message)}return data}
function fillSettings(){const s=STATUS.settings||{};document.getElementById('handleEnabled').checked=s.handle_enabled!==false;document.getElementById('usageFeedback').checked=s.enable_usage_feedback!==false;document.getElementById('enableResetProbe').checked=s.enable_reset_probe===true;document.getElementById('probeOnProvisionalRoster').checked=s.probe_on_provisional_roster===true;document.getElementById('monthlyMode').value=s.monthly_mode||'expiry_order';document.getElementById('scheduleAcrossPriorities').checked=s.schedule_across_priorities!==false;document.getElementById('enableManagedQuotaDisable').checked=s.enable_managed_quota_disable===true;document.getElementById('lifecycleEventLimit').value=s.lifecycle_event_limit||50;document.getElementById('refreshInterval').value=s.quota_refresh_interval||'30m0s';document.getElementById('staleAfter').value=s.stale_after||'5h0m0s';document.getElementById('refreshActiveWindow').value=s.refresh_active_window||'1h0m0s';document.getElementById('refreshAfterResetDelay').value=s.refresh_after_reset_delay||'1m0s';document.getElementById('refreshRetryDelays').value=s.refresh_retry_delays||'1m0s,5m0s,15m0s';document.getElementById('authFailureRetryInterval').value=s.auth_failure_retry_interval||'30m0s';document.getElementById('refreshOnStartup').checked=s.refresh_on_startup===true;document.getElementById('maxConcurrency').value=s.max_refresh_concurrency||1;document.getElementById('circuitFailureThreshold').value=s.circuit_failure_threshold||5;document.getElementById('circuitOpenDuration').value=s.circuit_open_duration||'30m0s';document.getElementById('circuitHalfOpenSuccessThreshold').value=s.circuit_half_open_success_threshold||2;document.getElementById('maxLogEntries').value=s.max_log_entries||200;document.getElementById('logRetention').value=s.log_retention||'24h0m0s';fillRetrySettings();applyDurationSplit()}
async function saveSettings(){try{if(!statusLoaded){await loadStatus();return}await requestManagement('/settings',{method:'PUT',body:collectSettingsPayload()});settingsDirty=false;showNotice(t('notice.settingsSaved'),false,'notice.settingsSaved');await refreshStatus({management:true,fillSettings:true})}catch(error){showNotice(error.message||String(error),true)}}
async function refreshQuota(){try{await requestManagement('/refresh',{method:'POST'});showNotice(t('notice.refreshRequested'),false,'notice.refreshRequested');await refreshStatus({management:true});pollStatus(3,1200)}catch(error){showNotice(error.message||String(error),true)}}
function splitTags(text){return text.split(',').map((item)=>item.trim()).filter(Boolean)}
function openEdit(authID){if(!hasManagementKey()){showNotice(t('error.managementKeyRequired'),true,'error.managementKeyRequired');return}const account=accountsByID.get(authID)||{};editingAuthID=authID;document.getElementById('editAuthID').textContent=authID;document.getElementById('editAlias').value=account.alias||'';document.getElementById('editSchedulerPriority').value=account.scheduler_priority||0;document.getElementById('editNotes').value=account.notes||'';document.getElementById('editGroupID').value=account.group_id||'';document.getElementById('editGroupName').value=account.group||'';document.getElementById('editGroupNotes').value=account.group_notes||'';document.getElementById('editTags').value=(account.tags||[]).join(', ');editDialog.showModal()}
function fillGroupFromID(){const groupID=document.getElementById('editGroupID').value.trim();const group=groupsByID.get(groupID);if(!group)return;if(!document.getElementById('editGroupName').value.trim())document.getElementById('editGroupName').value=group.name||'';if(!document.getElementById('editGroupNotes').value.trim())document.getElementById('editGroupNotes').value=group.notes||''}
async function saveAccountModal(){if(!editingAuthID)return;const groupID=document.getElementById('editGroupID').value.trim();const groupName=document.getElementById('editGroupName').value.trim();const groupNotes=document.getElementById('editGroupNotes').value.trim();const schedulerPriority=document.getElementById('editSchedulerPriority').valueAsNumber;if(!Number.isSafeInteger(schedulerPriority)){showNotice(t('error.schedulerPriorityInteger'),true,'error.schedulerPriorityInteger');return}try{await requestManagement('/annotations/account',{method:'PATCH',body:{auth_id:editingAuthID,alias:document.getElementById('editAlias').value,notes:document.getElementById('editNotes').value,tags:splitTags(document.getElementById('editTags').value),group_id:groupID,scheduler_priority:schedulerPriority}});const existingGroup=groupsByID.get(groupID)||{name:'',notes:''};if(groupID&&(groupName!==existingGroup.name||groupNotes!==existingGroup.notes)){await requestManagement('/annotations/group',{method:'PATCH',body:{id:groupID,name:groupName,notes:groupNotes}});groupsByID.set(groupID,{name:groupName,notes:groupNotes})}showNotice(t('notice.accountSaved'),false,'notice.accountSaved');editDialog.close();await refreshStatus()}catch(error){showNotice(error.message||String(error),true)}}
async function refreshOneQuota(authID){if(!authID)return;try{await requestManagement('/refresh/account',{method:'POST',body:{auth_id:authID}});showNotice(t('notice.refreshOneRequested'),false,'notice.refreshOneRequested');await refreshStatus();pollStatus(3,1200)}catch(error){showNotice(error.message||String(error),true)}}
async function exportConfig(){try{const data=await requestManagement('/export');const blob=new Blob([JSON.stringify(data,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-config.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.configExported'),false,'notice.configExported');await refreshLogs()}catch(error){showNotice(error.message||String(error),true)}}
async function exportLogs(){try{await refreshStatus({management:true});const payload={plugin_id:STATUS.plugin_id||'codex-quota-scheduler',exported_at:new Date().toISOString(),logs:STATUS.logs||[]};const blob=new Blob([JSON.stringify(payload,null,2)+'\n'],{type:'application/json'});const link=document.createElement('a');link.href=URL.createObjectURL(blob);link.download='codex-quota-scheduler-logs.json';link.click();URL.revokeObjectURL(link.href);showNotice(t('notice.logsExported'),false,'notice.logsExported')}catch(error){showNotice(error.message||String(error),true)}}
async function importConfigFile(file){if(!file)return;try{const text=await file.text();await requestManagement('/import',{method:'POST',body:text});settingsDirty=false;showNotice(t('notice.configImported'),false,'notice.configImported');await refreshStatus({management:true,fillSettings:true})}catch(error){showNotice(error.message||String(error),true)}}
function formatLogTime(value){if(!value)return'';const date=new Date(value);if(Number.isNaN(date.getTime()))return value;return date.toLocaleString(dateLocale(),{hour12:false})}
function resetRemainingText(date){const remaining=date.getTime()-Date.now();if(remaining<=0)return currentLocale==='en'?'expired':'已到期';const minute=60000;const hour=60*minute;const day=24*hour;if(remaining<day){if(remaining<hour){const value=Math.max(1,Math.floor(remaining/minute));return currentLocale==='en'?value+' min':value+'分钟'}const value=Math.floor(remaining/hour);return currentLocale==='en'?value+' hr':value+'小时'}const value=Math.floor(remaining/day);return currentLocale==='en'?value+' days':value+'天'}
function formatQuotaResetLabels(){for(const node of document.querySelectorAll('.quota-reset-label[data-label]')){const label=translateInlineText(node.dataset.label||'额度');node.textContent=currentLocale==='en'?label+' reset: ':label+' 重置：'}}
function formatLocalTimes(){formatQuotaResetLabels();for(const node of document.querySelectorAll('.localTime[data-time]')){const raw=node.dataset.time||'';const date=new Date(raw);if(Number.isNaN(date.getTime()))continue;const local=date.toLocaleString(dateLocale(),{hour12:false});if(node.classList.contains('quotaResetTime')){const remaining=resetRemainingText(date);node.textContent=currentLocale==='en'?local+' ('+remaining+')':local+' （'+remaining+'）'}else node.textContent=local}}
function localizedLogMessage(log){const key='log.'+(log.event||'');const dictionary=TRANSLATIONS[currentLocale]||TRANSLATIONS.en;if(dictionary[key])return dictionary[key];if(currentLocale==='zh-CN')return log.message||TRANSLATIONS.en[key]||'';return TRANSLATIONS.en[key]||translateInlineText(log.message||'')}
let logFilter='all'
function renderLogs(logs){const items=(logs||[]).slice().reverse().slice(0,80);renderLogChips(items);renderLogList(items.filter((entry)=>logFilter==='all'||entry.level===logFilter))}
function renderLogChips(items){const chips=document.getElementById('logChips');if(!chips)return;chips.replaceChildren();const counts={all:items.length,info:0,warn:0,error:0};for(const entry of items){if(counts[entry.level]!==undefined)counts[entry.level]++}const order=['all','info','warn','error'];for(const key of order){const label=key==='all'?t('logs.filterAll'):key==='info'?t('logs.filterInfo'):key==='warn'?t('logs.filterWarn'):t('logs.filterError');const chip=node('button','logChip'+(logFilter===key?' active':'')+(key==='error'?' danger':''),label+' '+counts[key]);chip.type='button';chip.dataset.level=key;chip.addEventListener('click',()=>{logFilter=key;renderLogs(STATUS.logs||[])});chips.append(chip)}}
function renderLogList(items){const list=document.getElementById('logList');list.replaceChildren();if(items.length===0){const empty=document.createElement('div');empty.className='empty';empty.textContent=t('logs.empty');list.appendChild(empty);return}for(const log of items){const row=document.createElement('div');row.className='logItem '+(log.level||'info');const meta=document.createElement('div');meta.className='logMeta';meta.textContent=[formatLogTime(log.time),log.event].filter(Boolean).join(' · ');const msg=document.createElement('div');msg.className='logMsg';const fields=log.fields?Object.entries(log.fields).map(([key,value])=>key+'='+value).join(currentLocale==='en'?', ':'，'):'';const message=localizedLogMessage(log);msg.textContent=fields?(currentLocale==='en'?message+' ('+fields+')':message+'（'+fields+'）'):message;row.append(meta,msg);list.appendChild(row)}}
async function refreshLogs(){try{await refreshStatus({management:true})}catch(error){showNotice(error.message||String(error),true)}}
localeSelect.addEventListener('change',()=>changeLocale(localeSelect.value));
document.getElementById('managementKey').addEventListener('input',syncRememberedManagementKey);
document.getElementById('managementKey').addEventListener('keydown',(event)=>{if(event.key==='Enter'){syncRememberedManagementKey();loadStatus()}});
document.getElementById('rememberManagementKey').addEventListener('change',syncRememberedManagementKey);
document.getElementById('loadData').addEventListener('click',loadStatus);
document.getElementById('saveSettings').addEventListener('click',saveSettings);
document.getElementById('refreshQuota').addEventListener('click',refreshQuota);
document.getElementById('refreshLogs').addEventListener('click',refreshLogs);
document.getElementById('exportLogs').addEventListener('click',exportLogs);
document.getElementById('exportConfig').addEventListener('click',exportConfig);
document.getElementById('importConfig').addEventListener('click',()=>document.getElementById('importFile').click());
document.getElementById('importFile').addEventListener('change',(event)=>importConfigFile(event.target.files&&event.target.files[0]));
document.getElementById('saveAccount').addEventListener('click',saveAccountModal);
settingsPanel.addEventListener('input',()=>{settingsDirty=true});
settingsPanel.addEventListener('change',()=>{settingsDirty=true});
retryPanel.addEventListener('input',()=>{settingsDirty=true});
retryPanel.addEventListener('change',()=>{settingsDirty=true});
document.getElementById('retryShadow').addEventListener('change',syncRetryModes);
document.getElementById('retryAlways').addEventListener('change',syncRetryModes);
document.getElementById('retryAddRow').addEventListener('click',()=>addRetryChainRow());
document.getElementById('retryReloadCatalog').addEventListener('click',loadRetryCatalog);
const PAGE_KEYS=['overview','logs','settings','retry']
function currentPage(){const hash=location.hash.replace('#','');return PAGE_KEYS.includes(hash)?hash:'overview'}
function showPage(page){if(!PAGE_KEYS.includes(page))page='overview';for(const section of document.querySelectorAll('[data-page-section]'))section.hidden=section.dataset.pageSection!==page;for(const button of document.querySelectorAll('.navItem')){const active=button.dataset.page===page;button.classList.toggle('active',active);if(active)button.setAttribute('aria-current','page');else button.removeAttribute('aria-current')}if(page==='retry'&&!retryCatalog.loaded)loadRetryCatalog()}
for(const button of document.querySelectorAll('.navItem'))button.addEventListener('click',()=>showPage(button.dataset.page))
window.addEventListener('hashchange',()=>showPage(currentPage()));
showPage(currentPage());
document.getElementById('retryCheckCPA').addEventListener('click',checkAndFixCPASettings);
document.getElementById('retrySave').addEventListener('click',saveRetrySettings);
document.getElementById('retryRefreshStats').addEventListener('click',refreshRetryStats);
document.getElementById('editGroupID').addEventListener('input',fillGroupFromID);
document.getElementById('editGroupID').addEventListener('blur',fillGroupFromID);
document.getElementById('closeDialog').addEventListener('click',()=>editDialog.close());
document.getElementById('cancelEdit').addEventListener('click',()=>editDialog.close());
for(const button of document.querySelectorAll('.openEdit')){button.addEventListener('click',()=>openEdit(button.dataset.authId||''))}
for(const button of document.querySelectorAll('.refreshOne')){button.addEventListener('click',()=>refreshOneQuota(button.dataset.authId||''))}
for(const button of document.querySelectorAll('.pinAccount')){button.addEventListener('click',()=>togglePin(button.dataset.authId||'',button.dataset.pinned==='true',button))}
const rememberedManagementKey=restoreRememberedManagementKey();
watchParentLocale();
rebuildDerivedState();
fillSettings();
renderMetrics();
renderRosterLifecycle();
renderAccounts(STATUS.accounts||[]);
formatLocalTimes();
applyLocale();
updateProtectedVisibility();
if(statusLoaded)startStatusPolling();
else if(rememberedManagementKey)loadStatus();
if(statusLoaded)refreshRetryStats();
</script>
</body>
</html>`))

func jsonForTemplate(v any) template.JS {
	raw, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(raw)
}
