# Codex 额度调度器 · 前端 UI 开发与视觉还原规范

> **视觉稿与开发交接版 v1.0** · 2026-10-21  
> 需求基线：用户提供的 `ui-design-brief.md`，产品版本 v0.4.0（v4 界面）。  
> 范围：**只重设计前端视觉与交互，不改变后端 API、业务逻辑或账号调度规则。**

## 0. 先看什么、交付了什么

本交接包包含两种图：

- `00-imagegen-visual-direction.png`：先由 Image 能力生成的视觉方向图。**仅用于确认总体美术方向**。因为 AI 图里的细小文字可能失真，不能当成字段规范或像素级验收图。
- `01-overview.png`、`02-logs.png`、`03-settings.png`、`04-retry.png`：**同一份 HTML/CSS 的 Chromium 实际渲染截图**，作为前端视觉验收的权威参考。
- `04a-retry-first-screen.png`：重试页首屏局部。
- `04b-retry-advanced.png`：展开 CPA 前置检查和高级参数后的状态。
- `05-edit-dialog.png`：编辑账号弹窗。
- `06-mobile-overview.png`：430px 窄屏布局。
- `codex-quota-ui-prototype.html`：完整无外部资源的原生 HTML/CSS/JavaScript **可交互视觉原型**，双击即可预览。内置演示数据，不发起真实网络请求。
- `original-ui-design-brief.md`：原始需求文档副本。

**建议开发方式：保留现有后端与 API 调用层，复用这份 HTML 中的 CSS、语义 DOM 结构与 render 函数，把原项目真实数据经过适配后传入。** 不必用图片切图还原，也不必引入 Vue、React、Tailwind 或图标 CDN。

注意：HTML 原型虽可交互，但 `pin/refresh/save/preflight` 等动作仅修改内存中的演示数据。它不是已经连接 CPA 的正式可部署产品。正式接入时，操作须调用原有 v4 端点并以后端确认的结果为准。

## 1. 产品信息架构（不可更改）

| 路由 | 页面 | 核心内容 | 首屏设计目标 |
|---|---|---|---|
| `#overview` | 账号总览（默认） | 下一账号、Monthly 模式、最近选择；账号队列卡片 | 不查表即可看到下一账号、可用性、剩余额度和固定/编辑按钮 |
| `#logs` | 日志 | 级别筛选与计数；倒序最多 80 条；导出 JSON | 保持表格高可读性、对齐技术字段 |
| `#settings` | 调度设置 | 常规、额度刷新、失败与恢复、熔断器、日志与历史共五组 | 危险选项与普通开关视觉分离，帮助文案不能省略 |
| `#retry` | 模型重试链 | 四模式、重试链编辑器、CPA 前置检查、高级参数、运行记录 | 模型起点和备用目标的关系非常清楚 |

页面刷新保持 hash 路由；未知 hash 改回 `#overview`。仅已加载状态进入四页。初始未加载状态只保留侧栏（含密钥与加载按钮），主区隐藏，不提前显示任何账号数据。

## 2. 视觉定位与页面尺寸

### 2.1 风格关键词

**轻量开发者运维台 / 高信息密度但不拥挤 / 蓝色聚焦 / 状态色克制 / 无装饰性插画。** 总体采用深海军蓝侧栏、冰灰主背景、白色业务卡片。主色蓝只用于调度优先级、当前导航、主按钮与聚焦；绿色用于正常/可用；琥珀用于临界/风险；红色用于错误/耗尽。

不使用玻璃拟态大面积透明背景、霓虹光效、渐变进度条、全屏图表或巨型品牌横幅。界面给运维者使用，不是营销落地页。

### 2.2 精确布局（CSS px）

| 区域 | 设计参数 | 实现说明 |
|---|---|---|
| 桌面侧栏 | 宽 **240px**，高度 `100vh` | `position: sticky; top:0`；左侧不随内容长页滚出 |
| 主区域 | 弹性填满宽度，最大 **1750px** | 外边距 auto；`padding:29px 34px 54px` |
| 主背景 | `#F5F7FB` | 与白卡区分但对比不过度 |
| 页面标题 | **25px / 780** | 左上，副说明 12.5px，底部与首个组件约 22px |
| 指标条 | 三列，gap **16px** | 每卡最小高度约 99px，左右内边距约 19px |
| 账号队列 | 桌面三列，gap **16px** | 单卡 `padding:17px 17px 13px`，长卡由内容自然撑开 |
| 普通业务卡 | `radius:15px`，边框 1px | `box-shadow` 为极轻阴影，不能用重浮层阴影 |
| 普通按钮 | 高度 ≥ **35px**，圆角 **8px** | 主按钮蓝底白字；次按钮白底灰边 |
| 紧凑按钮 | 高度 ≥ **29px** | 账号卡底操作及重试目标排序使用 |
| 行分割线 | 1px `#E2E8F1` | 表格行、表单行和卡片内结构 |
| 通用间距 | 4 / 8 / 12 / 16 / 20 / 24 / 32 / 40 | 尽量不随意混用不规则值 |

建议视觉验收以 **1600px 宽、980px 高** 的桌面窗口进行，再单独检查 **430px** 的移动布局。参考截图来源于 Chromium 的原生布局渲染（不是图片生成模型重绘）。

### 2.3 CSS 色彩 Token（与原型相同）

```css
:root {
  --bg: #f5f7fb;
  --surface: #ffffff;
  --surface-subtle: #f8fafd;
  --sidebar: #111d30;
  --sidebar-2: #172840;
  --sidebar-active: #214c8f;
  --ink: #17233a;
  --muted: #67758e;
  --subtle: #8c98aa;
  --line: #e2e8f1;

  --primary: #2563eb;
  --primary-hover: #1d4ed8;
  --primary-wash: #eff5ff;
  --success: #12a879;
  --success-wash: #e5f8f0;
  --warning: #e39413;
  --warning-wash: #fff5e3;
  --danger: #e44b59;
  --danger-wash: #fff0f1;
  --neutral-wash: #eef2f7;

  --radius-sm: 7px;
  --radius-md: 11px;
  --radius-lg: 15px;
  --sidebar-w: 240px;
}
```

字体：`-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", Arial, sans-serif`。技术 ID、模型名、事件名、数字推荐 `ui-monospace, SFMono-Regular, Consolas, monospace` 并启用表格数字排列。不得从 CDN 引入网页字体。原型 CSS 已预留 `[data-theme="dark"]` 变量，但暗色切换不是本轮功能范围，无须擅自增加设置入口。

### 2.4 视觉状态语义

| 状态 | 颜色/形态 | 注意 |
|---|---|---|
| 当前菜单 | 侧栏蓝色圆角背景 + 细左边强调线 | 配 `aria-current="page"` |
| 下一优先 | 白卡完整蓝色描边、`下一优先` 蓝色描边标签 | 不改变业务排序 |
| 固定 (999) | 蓝色浅色 pill `📌 999`，固定按钮改「取消固定」 | 优先级 999 是**插件优先级**，不是 CPA 优先级 |
| 额度充足 | 绿色进度条 / 浅绿可用标签 | 由**剩余量**决定而不是已用量 |
| 额度临界 | 琥珀色进度条 | 剩余额度在 30–60% |
| 额度耗尽 | 红色条 / 红色原因标签 | 在标签正文里内联恢复时间 |
| 数据缺失 | 灰色条 +「暂无数据」 | 不可当成 0% 使用量 |
| 登录失效 | 红色「请重新登录」 | 不与额度耗尽混用 |
| 熔断 | 等待/半开/全开独立徽章 | 文本必须跟真实状态匹配 |
| 高风险设置 | 琥珀标题区、⚠ 标记、帮助文案、必要时确认 | 后两项默认关，不能为了页面美观默认开启 |

## 3. 全局壳层和交互规范

侧栏顶端品牌区由蓝底简洁的内联 SVG 图形、产品名 `Codex 额度调度器` 与一句副标题组成；左侧四个导航项始终可见。侧栏连接区置于底部，用横线与导航分隔，依次包含语言选择、密码输入、记住密钥复选框、明文存储风险说明、`加载数据` 或 `刷新额度`、后台自动运行提示。

密钥输入框必须为 `type="password"`。默认不保存；仅用户明确勾选后保存在浏览器 `localStorage`，取消勾选立即删除该键。不在日志、卡片、toast 或导出文件内泄露密钥、OAuth token 或认证文件内容。生产环境必须沿用 v4 已有管理密钥鉴权方式，不以视觉原型的 mock 密钥逻辑替代服务端校验。

主区域右上有连接状态与原型 `UI DEMO` 小标签。`UI DEMO` 只存在于交接原型，接入真实运行时应移除，并以实际连接状态替换。页面最顶层 toast 成功显示 **4.5s**、失败 **8s**，可主动关闭；错误不能仅靠颜色传递，同时显示文字原因。`roster` 未确认时在主区顶部显示黄色警告条，不能在风险没解除时当正常连接处理。

语言支持 `zh-CN` / `en`，通过原系统语言继承或用户手动切换并记住。所有按钮、空态、错误、徽章、单位、表格、辅助说明都要翻译；来自后台的用户自定义别名与备注不翻译。所有绝对日期与 `reset_at` 必须按浏览器本地时区展示，不在界面中硬编码 +08:00。

## 4. P1 账号总览：开发细节

### 4.1 顶部指标

三张横向指标卡，分别为：下一账号（`nextId`，使用等宽字）、Monthly 使用模式、最近选择（`lastSelected`，使用等宽字）。每卡左侧一个浅色方形图标底板，右侧小标题、主值、短说明。不要把「最近选择」误写成「当前绑定」，它是**上一次实际选中的账号**。

### 4.2 账号卡片结构（固定顺序）

每个卡片按以下 DOM 纵向顺序渲染：

1. `.account-top`：账号图形 + 显示名（alias 优先，缺省 auth ID）、`auth:xxxx`、右上 rank `#N`。
2. `.attr-line`：分组 pill、任意多个自由标签 pill、`📌 999` 固定标签（条件）。
3. `.tech-line`：`CPA 优先级` 与 `插件优先级`，数字明确分开。
4. `.badge-row`：下一优先、可用/不可用原因、weekly/monthly/未知、熔断、登录、到期刷新等条件徽章。
5. 两组 `.quota-row`：5 小时窗口，周/月长窗口。每组包含窗口名、**剩余百分比**、距重置时间、水平进度条。
6. `.account-meta`：缓存年龄、数据来源（响应观测/轮询）、主动重置剩余/总获得次数，以及托管停用、调度说明、最后错误、下次重试等可选项。
7. `.account-notes`：账号备注及分组备注，仅有文本时呈现。
8. `.card-actions`：固定/取消固定、刷新额度（单账号）、编辑。

**原数据字段必须完整保留在 View Model 中**：

```js
{
  rank, auth_id, alias, notes, group_id, group, group_notes,
  tags: [], cpa_priority, scheduler_priority, family,
  available, unavailable_reason, cache_age, quota_source,
  managed_disable: { state, disabled_at, next_check_at },
  last_error, refresh_due_reason, next_retry_text,
  auth_failure, status_note,
  five_hour: { used_percent, remaining_percent, reset_at, exhausted, missing },
  long_window: { used_percent, remaining_percent, reset_at, exhausted, missing },
  circuit: { state, label, failure_count, success_count, next_probe },
  reset_credits_available_count, reset_credits_total_earned_count,
  reset_credits: [{ status, expires_at }]
}
```

上面是从原需求继承的账号字段集合，不代表 `/status` 响应只有这些字段，也不代表所有字段在每张卡上必须始终可见。缺失的条件字段不绘制，且前端**不负责推测账号是否可用**：`available`、`rank`、`next account` 等结果应该遵循服务端实时返回。

### 4.3 额度条规则（最容易写错）

颜色取决于 `remaining_percent`：**大于 60% 绿；30% 至 60%（含边界）琥珀；低于 30% 红**；缺失时灰色并显示「暂无数据」。进度条宽度是剩余量，而不是已使用量。字段同时包含 `used_percent` 和 `remaining_percent`，卡片同一行明确展示「已用 X% · 剩 Y%」，进度条突出剩余量。

```js
function quotaTone(remaining, missing) {
  if (missing || remaining == null) return 'missing';
  if (remaining > 60) return 'green';
  if (remaining >= 30) return 'amber';
  return 'red';
}

function localResetText(resetAt) {
  // resetAt 为绝对时间；相对时间「剩3小时」应基于 Date.now() 动态更新。
  // title/tooltip 显示 new Date(resetAt).toLocaleString()，不要手算固定时区。
}
```

后端若传回已耗尽标记应优先遵守服务端状态；不应仅凭显示百分比四舍五入为 0 就私自标记耗尽。`missing` 与真实 `0%` 是两种不同状态。

### 4.4 卡片操作与排序

点击固定/取消固定：发送原 v4 已有动作 → 以后端确认的优先级和队列 rank 更新卡片 → 标识本次执行结果。原型内为了展示交互用了本地重排算法，**不是正式调度算法，不要复制到线上**。账号级刷新只更新目标账号的额度信息，不能重置当前的设置草稿。编辑按钮打开对话框；保存结果以后端为准。

账号为空：显示文案「暂无账号数据。等待额度刷新后，这里会显示账号卡片。」并保持页面其余壳层稳定。

## 5. P2 日志：开发细节

筛选行 `全部 N | info N | warn N | error N` 的计数基于**当前日志全集**，点击芯片仅过滤列表，不能改变全集计数。操作区：`刷新日志`、`导出日志（JSON）`。表格 5 列：**本地时间 / 等级 / 事件名 / 消息 / 字段**。事件名和字段键值用等宽字体，例如 `quota.refresh_success`、`auth_id=main-team`；消息随语言转换。

日志按时间倒序，**界面最多 80 条**。需要横向空间时对表格本身 `overflow-x:auto`，而不是把整个页面撑宽。空态文案：「暂无日志。发起请求或手动刷新额度后，这里会显示记录。」导出 JSON 时是否导出后端完整保留集，应跟原 v4 逻辑一致，不能误以为前端显示 80 条意味着后端只保存 80 条。当前原型导出最多 80 条的演示列表，不构成正式数据策略。

```html
<div class="toolbar logs-toolbar">
  <div class="filters" role="group" aria-label="日志级别">
    <button class="filter is-active" aria-pressed="true">全部 <b>80</b></button>
    <button class="filter" aria-pressed="false">● info <b>52</b></button>
    <button class="filter" aria-pressed="false">▲ warn <b>18</b></button>
    <button class="filter" aria-pressed="false">✕ error <b>10</b></button>
  </div>
  <div class="toolbar-actions">
    <button class="btn">刷新日志</button>
    <button class="btn">导出日志 (JSON)</button>
  </div>
</div>
```

上例数字只是视觉示例，实际必须来自返回数据。

## 6. P3 调度设置：21 项字段和样式

### 6.1 布局与保存

图片 `03-settings.png` 中五组设置各是独立白卡；组头固定结构为 **33×33px 淡色图标底板 + 14px 粗体名称 + 11.4px 说明**。每行由左侧字段名称（12px/720）与帮助文案（10.6px/1.65）以及右侧表单控件组成。控件列占约 33% 宽度；较长字段可换行，不能剪掉帮助文案。

桌面按视觉平衡排两列：左列「常规 → 失败与恢复 → 日志与历史」，右列「额度刷新 → 熔断器」；窄屏叠为一列。两列是视觉布局而不是业务优先级。危险组用琥珀色图标底与脚注，重要风险通过 ⚠ 文字符号明确传达，不能仅用黄色区别。

顶部蓝色说明条固定业务文案：「默认配置已经过优化，正常情况下无需手动调整；修改后记得保存。」在 `自动激活新的额度周期` 关闭且已加载时添加黄色解释警告。底部单独的 `导出配置 / 导入配置 / 保存设置` 操作行，右对齐；设置页保存**绝不同时提交**重试页尚未保存的修改。表单发生变更时可见「有未保存的设置修改」。

### 6.2 21 项控件契约

表中 `viewKey` 是 **本交接原型在 JavaScript 中使用的字段名**。原文仅定义了用户可见字段及概念，没有给 `/settings` 实际 JSON 键名；对接时必须从 v4 现有代码确认映射，不可直接把这些 `viewKey` 当作后端字段名。

| 组 | viewKey | 显示名称 | UI 类型 | 默认值 / 显示 |
|---|---|---|---|---|
| 常规 | `scheduler_enabled` | 启用调度接管 | 开关 | 开 |
| 常规 | `fallback_lower_priority` | 更高优先级耗尽时使用较低优先级账号 | 开关 | 开 |
| 常规 | `monthly_mode` | 月度账号使用方式 | 双单选 | 按到期时间排序 |
| 额度刷新 | `refresh_interval` | 额度刷新间隔 | 数值 + 单位 | 30 分钟 |
| 额度刷新 | `cache_expiration` | 缓存过期判定 | 数值 + 单位 | 5 小时 |
| 额度刷新 | `active_refresh_window` | 活跃刷新窗口 | 数值 + 单位 | 1 小时 |
| 额度刷新 | `reset_refresh_delay` | 重置后刷新延迟 | 数值 + 单位 | 1 分钟 |
| 额度刷新 | `auth_failure_retry_interval` | 认证失败重试间隔 | 数值 + 单位 | 30 分钟 |
| 额度刷新 | `failure_retry_sequence` | 刷新失败重试间隔 | 文本，逗号分隔 | `1m,5m,15m` |
| 额度刷新 | `refresh_on_start` | 启动时刷新额度 | 开关 | 关 |
| 额度刷新 | `max_concurrent_refresh` | 最大并发刷新 | 正整数 | 1 |
| 失败与恢复 | `feedback_marks_exhausted` | 失败反馈标记额度耗尽 | 开关 | 开 |
| 失败与恢复 | `activate_new_reset` | 自动激活新的额度周期 ⚠ | 开关 | 关 |
| 失败与恢复 | `probe_unconfirmed_roster` | 账号列表未确认时仍允许额度探测 ⚠ | 开关 | 关 |
| 失败与恢复 | `managed_disable` | 额度耗尽时在 CPA 层停用账号并自动恢复 ⚠ | 开关 | 关 |
| 熔断器 | `circuit_failure_threshold` | 熔断失败阈值 | 正整数 | 5 |
| 熔断器 | `circuit_wait` | 熔断等待时间 | 数值 + 单位 | 30 分钟 |
| 熔断器 | `circuit_halfopen_successes` | 半开恢复成功次数 | 正整数 | 2 |
| 日志与历史 | `max_logs` | 最大日志条数 | 正整数 | 200 |
| 日志与历史 | `log_retention` | 日志保留时间 | 数值 + 单位 | 24 小时 |
| 日志与历史 | `managed_disable_history_max` | 托管停用历史条数上限 | 正整数 | 50 |

**字段下方的正式中文帮助文案不得丢弃或自行概括。** 完整文本以 `original-ui-design-brief.md` 第五节为定稿；原型源代码中的 `groups[]` 已逐项提供中文/英文说明。上线接入时，若项目已有国际化字典，优先复用现有翻译，而不是建立相互矛盾的第二套键。

### 6.3 数值 + 单位复合输入

UI 采用输入框（约 45%）加单位下拉（约 55%）的组合，左侧圆角 7px 0 0 7px，右侧圆角 0 7px 7px。单位可选「分钟 / 小时 / 天 / 原始」。Go `time.ParseDuration` **不直接接受 `1d`**；选 1 天时序列化为 `24h`，选 2 天为 `48h`。复杂或无法拆解的 Go duration（例如 `1h30m`）用「原始」文本输入显示，不能强制拆错后丢失精度。

```js
// 与交接原型一致的关键转换逻辑：
function buildDuration(value, unit) {
  if (unit === 'raw') return String(value);
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) throw Error('时长必须大于 0');
  if (unit === 'd') return `${n * 24}h`; // Go 不支持 d
  return `${n}${unit}`;                 // m、h；视情况支持 s
}

function isDuration(raw) {
  // 允许 Go 风格复合序列，如 1h30m、90s、500ms
  return /^(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$/u.test(raw);
}
```

后端如支持负时间、纳秒或复杂 `time.Duration` 表达式，解析器必须以**现有后端接受的实际格式**为准。原型演示版只对正时长进行常规验证。

### 6.4 高风险选项的交互细节

「自动激活新的额度周期」默认关闭，辅助文案必须明确它可能消耗少量额度。「账号列表未确认时仍允许额度探测」可能使用旧账号列表；「额度耗尽时在 CPA 层停用账号并自动恢复」会写宿主认证文件；这两项建议打开时二次确认，保留默认关闭。关闭风险选项不能阻止界面正常加载其他数据。

## 7. P4 模型重试链：完整业务区域

### 7.1 运行模式

首张宽卡为 **四选一分段控件**：`关闭 / 影子 / 生效 / 全局`。分段控件浅灰底、内部单个蓝色激活块、非激活选项深灰文本。选择后同步更新对应草稿状态，但不立即保存至后端。模式解释保持原文语义：影子只记录会重试的情况；生效仅链内模型重试；全局所有模型都重试。

### 7.2 重试链编辑器

每条链是一张浅灰内卡，采用 `原始模型 → 备用目标列表 → 操作` 的网格。最左一列为客户端请求模型（等宽文本输入）；中间用右向箭头说明映射；右侧每条备用目标包含**序号、提供方（可空）、模型名、上移、下移、删除**，可以多条；最右是删除整链与链条上下排序。页尾放 `添加模型` 和 `载入模型列表`。

提供方必须明确解释：**留空时由 CPA 自动选择；填写时需输入 CPA 的提供方键，例如 `codex` 或 `openai`。** 模型名建议来源为 CPA 动态加载，不要在正式代码里永久硬编码原型中的演示列表。某些链模式仅对链内模型生效，全局模式例外。

```html
<section class="panel content-panel" id="retry-editor">
  <div class="panel-title">
    <h2>重试链编辑器</h2>
    <p>客户端请求的模型 → 按顺序尝试的备用目标</p>
  </div>
  <div class="chains">
    <div class="chain-card">
      <div class="chain-origin">
        <label>客户端请求的模型</label>
        <input class="input mono" value="gpt-4o">
      </div>
      <div class="chain-arrow" aria-hidden="true">→</div>
      <div class="target-list">
        <div class="target-item">
          <span class="target-index">1</span>
          <input class="input mono" value="codex" aria-label="提供方">
          <input class="input mono" value="gpt-4o-mini" aria-label="模型名">
          <button aria-label="上移">↑</button>
          <button aria-label="下移">↓</button>
          <button aria-label="移除备用目标">×</button>
        </div>
      </div>
      <button aria-label="删除整条链">×</button>
    </div>
  </div>
</section>
```

```css
.chain-card {
  display: grid;
  grid-template-columns: minmax(148px,.92fr) 26px minmax(0,2.3fr) auto;
  align-items: start;
  gap: 12px;
  border: 1px solid var(--line);
  border-radius: 11px;
  background: var(--surface-subtle);
  padding: 13px;
}
.target-item {
  display: grid;
  grid-template-columns: 24px minmax(69px,.74fr) minmax(100px,1.3fr)
                         28px 28px 28px;
  gap: 5px;
  align-items: center;
}
```

对源模型重复、缺少模型名、空链但启用、目标链全部删除等情况，保存时进行校验，并以 toast / 内联红色提示反馈。拖拽排序**不是必要条件**；原型提供上移/下移按钮，键盘更容易操作，也能精确控制顺序。

### 7.3 CPA 前置检查

必须是独立卡片，与重试链编辑区之间有 16–17px 间距。首屏状态显示检查通过 / 需要升级 / 配置差异摘要，并提供 `检查并修复` 按钮。点击后展开一个带琥珀边框的确认区，按 `当前值 / 推荐值` 对照展示四项：

| 配置项 | 交互规则 |
|---|---|
| `request-retry` | 差异项红色当前值 |
| `buffering` | 差异项红色当前值 |
| `timeout` | 相同项正常/绿色 |
| `bootstrap-retries` | 相同项正常/绿色 |

确认区操作为「应用推荐设置」与「取消」。**图片中演示用的“当前值/推荐值”仅是 UI fixture；原 brief 没有提供四项的真实推荐数值或 CPA 版本兼容矩阵。必须调用现有管理端点获得真实值，不得把示意值直接写进正式逻辑。**

### 7.4 高级参数折叠区

默认折叠，在头部横向摘要中保留七个配置的关键值，建议字型 11px、颜色 `var(--muted)`：

`最大尝试 4 · 静默 60s · 等待 90s · 整链 240s · 帧 4096 · 字节 8MB · 移除加密推理 ✓`

展开为 2 列设置网格，包含：最大尝试次数 (1–16)、保留帧上限、单次静默超时、等待可提交内容上限、整条链总时限、保留字节上限、切换模型时移除加密推理内容。最后一项是开关，帮助文案需说明 Codex 加密推理含原模型签名，切换后无法验证，所以默认推荐开。

折叠头使用 `<details><summary>` 或同等的可访问实现。摘要字段值必须跟当前草稿同步更新，保存前仍标记 `retryDirty`。

### 7.5 运行记录与独立保存

运行统计 6 个小卡：模式、请求数、重试次数、重试后成功（绿）、重试后失败（红）、本应重试（影子，蓝）。桌面宽屏 6 列，窄屏 3/2 列。下方按时间倒序列出最近重试事件，包含时间、事件、技术字段。按钮「刷新记录」不能提交表单草稿。

页尾只有 `保存重试链设置`，不能复用设置页的 `保存设置` 事件处理器，也不能顺带提交设置页内未保存的更改。

## 8. 编辑账号弹窗和空态

`05-edit-dialog.png` 的对话框最大宽约 **600px**，圆角 15px，白底，黑色半透明背景遮罩（可轻微模糊），顶部固定标题和 auth ID，底部取消 / 保存按钮。内容七项依序是：**别名、插件优先级、分组 ID、分组名称、标签（逗号分隔）、账号备注（多行）、分组备注（多行）**。第一行和第二行桌面为两列；标签和备注独占整行。`999=置顶` 提示紧邻优先级字段。支持 Esc 关闭、关闭图标、提交后 toast。

所有来自用户、API 的字符串必须按**文本**插入 DOM，严禁直接拼接未经转义的 HTML。在此原型中使用 `esc(value)` 转义 `& < > " '`；正式接入建议使用 `textContent` / DOM API 承载动态文本。技术标识可使用等宽字体，但绝不显示 OAuth 凭证。

空态必须单独覆盖：未加载、账号列表为空、日志为空、重试链为空、请求失败/密钥错误、额度窗口缺失、管理端 roster 未确认。空态不能为了美观用虚构的真实数据填充；交接图里所有账号、模型、日志数据都是**演示 fixture**。

## 9. HTML/CSS 核心骨架（可直接复用）

完整实现已在 `codex-quota-ui-prototype.html`；下面是核心层次及卡片布局摘要，CSS 类名与原型相同：

```html
<div class="app-shell">
  <aside class="sidebar">
    <div class="brand"><!-- inline SVG + 品牌标题 --></div>
    <nav class="nav" aria-label="主导航">
      <a class="nav-item" href="#overview" aria-current="page">账号总览</a>
      <a class="nav-item" href="#logs">日志</a>
      <a class="nav-item" href="#settings">调度设置</a>
      <a class="nav-item" href="#retry">模型重试链</a>
    </nav>
    <div class="sidebar-bottom"><!-- 语言、密钥、加载/刷新 --></div>
  </aside>

  <main id="main" class="workspace">
    <header class="page-header"><!-- 标题、副标题、连接状态 --></header>
    <section class="metrics"><!-- 三个统计卡 --> </section>
    <section class="accounts-grid">
      <article class="panel account-card is-next">
        <div class="account-top"><!-- 标题、auth、rank --></div>
        <div class="attr-line"><!-- 分组与标签 --></div>
        <div class="tech-line"><!-- CPA / 插件优先级 --></div>
        <div class="badge-row"><!-- 状态徽章 --></div>
        <div class="quota-row"><!-- 5h 额度条 --></div>
        <div class="quota-row"><!-- 周/月额度条 --></div>
        <div class="account-meta"><!-- 缓存、来源、次数等 --></div>
        <div class="card-actions"><!-- 固定、刷新、编辑 --></div>
      </article>
    </section>
  </main>
</div>
<div id="toastArea" class="toast-area" aria-live="polite"></div>
<dialog id="editDialog" class="modal" aria-labelledby="editDialogTitle"></dialog>
```

```css
.app-shell {
  display: grid;
  grid-template-columns: var(--sidebar-w) minmax(0, 1fr);
  min-height: 100vh;
}
.sidebar {
  position: sticky;
  top: 0;
  height: 100vh;
  display: flex;
  flex-direction: column;
  background: linear-gradient(168deg, #142239 5%, #101c2e 80%);
}
.workspace {
  width: 100%;
  min-width: 0;
  max-width: 1750px;
  margin: 0 auto;
  padding: 29px 34px 54px;
}
.panel {
  background: var(--surface);
  border: 1px solid var(--line);
  border-radius: 15px;
  box-shadow: 0 2px 10px rgba(19,35,65,.035);
}
.accounts-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0,1fr));
  gap: 16px;
}
.account-card {
  display: flex;
  flex-direction: column;
  padding: 17px 17px 13px;
  min-width: 0;
}
.account-card.is-next {
  border-color: #538cf9;
  box-shadow: 0 0 0 1px #d0e2ff;
}
.card-actions {
  display: grid;
  grid-template-columns: 1.05fr 1.2fr .8fr;
  gap: 6px;
  margin-top: 12px;
}
```

页内图标只用内联 SVG（原型 `ic(name, size)` 有有限的白名单路径）。不得在 iframe 环境强依赖外部字体包、图片服务器或图标源。按钮、输入框、switch、badge 的 hover/focus/disabled/active 状态以原型 CSS 为实现依据，不再重复创造第二套视觉 Token。

## 10. 状态管理与后端 API 适配（**最重要的工程边界**）

### 10.1 前端状态必须分开

可以继续保留原生 JS，不引入框架。建议按下面的**概念**组织状态，不要求完全复制变量名：

```js
const uiState = {
  loaded: false,
  activeRoute: 'overview',
  language: 'zh-CN',
  secret: '',
  rememberSecret: false,

  // 真正的运行快照：轮询刷新。
  status: null,
  logs: [],
  retryRuntime: null,

  // 两套完全独立的「服务端已保存值」与「用户草稿」。
  settingsSaved: null,
  settingsDraft: null,
  settingsDirty: false,
  retrySaved: null,
  retryDraft: null,
  retryDirty: false,

  activeLogLevel: 'all',
  editingAuthId: null,
  toastQueue: []
};
```

**重要：**轮询新数据可以更新 `status`、日志、运行统计等，但如果 `settingsDirty === true`，禁止拿轮询回来的配置覆盖当前表单；同理 `retryDirty === true` 时不能覆盖重试草稿。设置页保存成功，仅同步 `settingsSaved ← settingsDraft`；重试页保存成功，仅同步 `retrySaved ← retryDraft`。失败应保留草稿并显示错误。

### 10.2 已确认与待确认的后端信息

原文明确提到读取来源 `/status`、`/settings`、`/retry/status`，但**没有详细描述**这些端点的 JSON 完整封装、全部写入端点的 HTTP 方法、管理密钥 header 名称、导入/导出服务端协议，以及 CPA 前置修复端点的请求体。**以上必须从 v4 实际代码中复用或验证，严禁根据示意 HTML 发明新的后端 API。**

前端可建立一层 `adapter`，仅负责把旧接口返回值变为页面 View Model；原请求逻辑由现有仓库提供：

```js
// 伪代码：existingV4Api 是原项目的已实现客户端，不是新接口。
// 其认证头、错误码、请求方法均使用原 v4 实现。
async function loadManagementSnapshot() {
  const [statusRaw, settingsRaw, retryRaw] = await Promise.all([
    existingV4Api.readStatus(),        // 原先 GET /status
    existingV4Api.readSettings(),      // 原先 GET /settings
    existingV4Api.readRetryStatus()    // 原先 GET /retry/status
  ]);

  uiState.status = normalizeStatus(statusRaw);
  uiState.retryRuntime = normalizeRetryStatus(retryRaw);
  uiState.settingsSaved = normalizeSettings(settingsRaw);
  if (!uiState.settingsDirty) {
    uiState.settingsDraft = structuredClone(uiState.settingsSaved);
  }
  // retryDraft 也只在未编辑时，由其对应配置接口的返回值初始化/更新。
  renderCurrentRoute();
}

async function saveSchedulingSettings() {
  validateSettings(uiState.settingsDraft);
  // serialiseSettings 必须与原 v4 后端字段、Go Duration 格式对应
  const payload = serialiseSettings(uiState.settingsDraft);
  await existingV4Api.saveSettings(payload);
  uiState.settingsSaved = structuredClone(uiState.settingsDraft);
  uiState.settingsDirty = false;
  showToast('保存设置成功', 'success');
}

async function saveModelRetrySettings() {
  validateRetry(uiState.retryDraft);
  await existingV4Api.saveRetryConfig(serialiseRetry(uiState.retryDraft));
  uiState.retrySaved = structuredClone(uiState.retryDraft);
  uiState.retryDirty = false;
  showToast('保存重试链设置成功', 'success');
}
```

上面的 `existingV4Api.*` 和 `normalize*` 是**适配层占位函数**，不是交接文档保证存在的真实函数名。开发时要对照现有 Go handler、内嵌页面及 v4 API 调用逐一落实。这项集成工作**不在本次纯视觉原型内完成**。

### 10.3 所有需要保留的动作

| 动作 | 预期触发 / 状态变化 | 失败策略 |
|---|---|---|
| 加载数据 | 按原管理密钥规则获取运行快照、设置、重试状态 | toast 8s + 密钥输入高亮，不显示不可信数据 |
| 手动刷新额度 | 请求原刷新动作；拉取最新服务端结果 | 提示具体错误，不清空本地编辑草稿 |
| 单账号刷新 | 只对目标 `auth_id` 操作 | 禁止误刷新或修改其他账号 |
| 置顶/取消 | 修改插件优先级 999/恢复优先级，然后以服务端 rank 重排 | 如果乐观更新失败，必须回滚或重新读取服务端队列 |
| 编辑账号 | 校验别名、优先级、分组、标签、备注后保存 | 弹窗内容保留，定位无效字段 |
| 过滤日志 | 仅本地过滤 | 不触发设置保存 |
| 刷新/导出日志 | 重新读取并导出符合原 v4 行为的 JSON | 网络故障提供 toast，不插入伪成功日志 |
| 导入/导出设置 | JSON 导入写入设置草稿；保存需用户再确认 | 无效 JSON / 未知键反馈错误，不提交重试配置 |
| 保存设置 | 仅提交调度设置草稿 | 保存失败不清除 dirty |
| 编辑模型链 | 添加、删除、排序模型及备用目标 | 禁止源模型重复或空的目标模型名 |
| 载入模型列表 | 从 CPA 实时获取提供方 + 模型建议 | 列表不可用时仍允许手动填写 |
| CPA 检查并修复 | 先展示差异确认块，再按原接口应用推荐配置 | 必须在用户确认后才有写操作 |
| 展开高级参数 | UI 本地状态切换 | 不提交任何草稿 |
| 刷新运行记录 | 只刷新重试事件与统计 | 不修改保存状态 |
| 保存重试链设置 | 仅提交重试配置草稿 | 保留设置页尚未保存的修改 |

### 10.4 前端并非账号调度引擎

真实优先级、可用性、窗口重置、429 避让、熔断、复活、CPA 凭证写入均发生在插件/CPA 后端。前端负责忠实**展示**服务端状态和发起受授权的管理动作，而不能仅凭前端计时、简单排序或百分比重新决定“下一账号”。尤其在账号数较多、CPA 优先级多层级、monthly/weekly 混合使用时，这点不可破坏。

## 11. 安全、国际化、无障碍与兼容性

### 11.1 安全

- 管理密钥 `type=password`，默认只存内存；选择「记住」后明文保存本地浏览器存储，风险提示常驻。
- 禁止把认证密钥、OAuth 凭证、Go auth 文件内容插入页面或 JSON 导出。
- 所有用户别名、自由标签、备注、服务器错误文本均必须转义，建议原生 `textContent`，不能直接不经转义地拼到 `innerHTML`。
- 受保护操作必须由原 CPA 管理接口在服务端完成鉴权，UI 显示“已登录”本身不是权限证明。
- 若 iframe 受 CSP 或 `frame-ancestors` 策略保护，保留 CPA 的已有安全策略；不要为能嵌入而放宽成任意站点。
- 账号启用/停用、列表未确认时探测等危险功能沿用原后端的安全校验，不因更换 UI 而绕过。

### 11.2 中英文与日期

原文要求的全部业务文案有 `zh-CN`/`en` 两套。英文通常比中文长 20–40%，设置表单标题要允许换行；例如 “Allow lower-priority accounts when higher tiers are exhausted” 不能放进固定 120px 宽的单行文本框。技术 ID 不翻译；用户输入内容保留原样；`reset_at`、日志发生时间、下次重试时间全部用 `new Date(...).toLocaleString()` 或 `Intl.DateTimeFormat` 结合浏览器时区显示。

### 11.3 无障碍

导航当前页使用 `aria-current="page"`；筛选芯片与模式选择用 `aria-pressed` 或等价的 Radio Group 语义；开关使用原生 checkbox + `role="switch"`；额度条使用 `role="progressbar"`，设置 min/max 和有效值；SVG 为装饰性时设 `aria-hidden="true"`；toast 容器 `aria-live="polite"`，失败消息可 `role="alert"`；弹窗用 `<dialog>`、聚焦首个输入项、支持 Esc；排序与删除有可读的按钮名称，不应只有一个无说明的图标。

交互控件需要可见 `:focus-visible` 焦点环；已支持 `prefers-reduced-motion`。警告、成功与失败不能只靠色彩区分，必须有文字或图标语义。

## 12. 响应式细则（按实际原型断点）

```css
@media (max-width: 1399px) {
  .accounts-grid { grid-template-columns: repeat(2, minmax(0,1fr)); }
  .retry-metrics { grid-template-columns: repeat(3, minmax(0,1fr)); }
}
@media (max-width: 1130px) {
  .settings-grid { grid-template-columns: 1fr; }
  .data-table { min-width: 800px; } /* 只允许日志表格横向滚动 */
}
@media (max-width: 859px) {
  .app-shell { display: block; }
  .sidebar { position: relative; height: auto; }
  .nav { display: flex; overflow-x: auto; }
  .workspace { padding: 20px 16px 30px; }
}
@media (max-width: 620px) {
  .accounts-grid, .metrics { grid-template-columns: 1fr; }
  .setting-row { grid-template-columns: 1fr; }
  .retry-metrics { grid-template-columns: repeat(2, minmax(0,1fr)); }
}
```

原需求的**硬断点是 `<860px` 侧栏折叠为顶部横条**；实现已在 859px 使用该规则。顶部导航允许横向滚动、当前项高亮；连接密钥区域在移动端折为紧凑的双列布局，仍保留密钥输入和「记住密钥」说明。移动端卡片独占一列，日志表格允许局部滚动。**整页不能出现横向滚动条。**

## 13. Go 单文件嵌入、目录与接入步骤

本原型已经是单个 HTML 文件，内部包含 CSS、JS、SVG 图标，不引用外部文件。正式接入可将 HTML 放进项目现有前端模板目录或 Go embed 目录；以下仅展示标准 Go `embed` 写法，**不规定新接口路由**：

```go
package ui

import _ "embed"

//go:embed scheduler-ui.html
var schedulerUI []byte

// 在原有 v4 UI handler 中返回此 HTML：
// w.Header().Set("Content-Type", "text/html; charset=utf-8")
// _, _ = w.Write(schedulerUI)
```

给正式开发 Agent 的建议执行顺序是：阅读本说明和原始 brief → 在原仓库定位 v4 嵌入页面与 `/status`、`/settings`、`/retry/status`、账号操作和前置修复的真实调用 → 拆出已有 API / i18n → 把原型的 CSS + DOM 渲染层替换进去 → 做响应式与交互回归 → 截图对照本交接图 → 最后再接入构建与 Go embed。不要先改动后端调度逻辑，也不要让前端直接修改 CPA auth 文件。

## 14. 验收清单（UI/QA）

| ID | 用例与期望结果 | 原型烟测状态 |
|---|---|---|
| A01 | 初始未加载：四个主页面内容隐藏，侧栏仍可输入密钥 | 已实现（mock） |
| A02 | 页面路由 `#overview/#logs/#settings/#retry`，未知路由回退 | 已实现 |
| A03 | 页面重载后 hash 保留；手动切换中英后语言保留 | 已实现 |
| A04 | 三个指标值布局对齐、不溢出 | 已截图校验 |
| A05 | account rank、auth ID、alias、分组、标签、双优先级可辨识 | 已截图校验 |
| A06 | 5h 与周/月剩余额度条分别显示；缺失≠零额度 | 已截图校验 |
| A07 | 剩余额度 61/60/30/29/0 对应绿/琥珀/琥珀/红/红 | 颜色规则已实现 |
| A08 | 不可用标签显示原因与恢复时间、登录失效红色提示 | 已截图校验 |
| A09 | Pin/unpin 使队列即时重排；生产以后端为准 | 原型仅本地模拟；真实 API 待对接 |
| A10 | 编辑对话框支持全部七项，Esc/取消/保存 | 已交互测试 |
| A11 | 使用 `<script>...` 作为别名不应执行 JS | 已交互测试：按文本显示 |
| A12 | 日志过滤、计数、时间与倒序、JSON 导出 | 原型实现；服务端数据待对接 |
| A13 | 五组设置共 21 项，所有帮助文案显示 | 已截图校验 |
| A14 | Go duration 的 1 天变为 `24h`，复合值原始保留 | 转换代码已实现 |
| A15 | 高风险项默认关闭，必要二次确认；禁用警告出现 | 已实现（演示） |
| A16 | 设置 Dirty 不被轮询覆盖；与重试 Dirty/保存隔离 | 跨页编辑与保存隔离已测试；真实轮询防覆盖待集成验证 |
| A17 | 四种重试模式互斥；目标可增删、上下排序 | 已实现（演示） |
| A18 | CPA 四项差异对照，点击应用前必须确认 | UI 已实现；推荐值/真实 API 待接入 |
| A19 | 高级参数七项可展开，最大尝试 1–16 验证 | 已实现 |
| A20 | 运行统计六项+事件记录、重试页独立保存 | 已实现（演示） |
| A21 | 1600px 桌面、430px 手机，页面无水平溢出 | Chromium 已校验，移动端溢出 0px |
| A22 | 中文/英文说明 20–40% 伸长后布局仍可读 | 需正式字体/浏览器组合回归 |
| A23 | 真正的连接失败/管理密钥错误/轮询异常 | **待真实 API 集成测试** |
| A24 | 真实置顶、账号刷新、预检修复、配置写入、后台轮询 | **待真实 API 集成测试** |
| A25 | iframe 运行、CPA 管理中心语言继承、原项目 CSP | **待集成宿主测试** |

本轮已经执行的**离线原型烟测**：Chromium 渲染四页 + 编辑弹窗 + 430px 移动端，浏览器运行时无未捕获 JS 异常；移动端 `document.body.scrollWidth - clientWidth = 0`；设置草稿修改后切换页面，保存重试配置不会清空设置草稿；尚未模拟真实后台轮询。该结果不是对 CPA 后端操作正确性的证明。

## 15. 最终交付判定标准

设计对齐看 `01`～`06` 真实页面截图，而非 Image 概念图里的细小字符；业务字段与中文文案看**原始 brief**；可复用的 DOM/CSS 和行为看**单文件 HTML 原型**；真实 API、后端安全校验和调度算法看**现有 v4 仓库**。四者职责独立且互相校验。

只有同时满足「视觉基本一致」「原文业务内容不缺」「操作能通过既有管理 API 生效」「不引入外部依赖或安全退化」四个条件，才能认为正式前端迁移完成。
