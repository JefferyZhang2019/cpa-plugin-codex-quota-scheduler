Codex 额度调度器 · UI 设计与开发交接包
====================================
原始依据：original-ui-design-brief.md（v0.4.0 / v4）

建议阅读顺序：
1. 00-imagegen-visual-direction.png —— image_gen 生成的概念风格，仅供审美参考
2. 00-implementation-contact-sheet.png —— 四页面真实 HTML 渲染总览
3. 01-overview.png / 02-logs.png / 03-settings.png / 04-retry.png
4. 05-edit-dialog.png / 06-mobile-overview.png / 04b-retry-advanced.png
5. codex-quota-ui-development-guide.md —— 完整前端开发交接说明
6. codex-quota-ui-prototype.html —— 无依赖 HTML/CSS/JS 可交互原型

双击 prototype.html 后，左侧点击「加载数据」进入演示界面；
也可以在 URL 后加 ?demo=1#overview 直接展示模拟账号卡片。

原型只使用演示数据，不会访问真实 CPA、不发送/储存真实 OAuth 凭证。
要部署上线，需要沿用原 v4 实际后端写入/读取 API 与管理密钥鉴权逻辑，
文档中明确指出不能从原需求推断的端点和载荷。

参考截图生成环境：Chromium 1600x980、Asia/Shanghai；移动端 430px。
所有 CSS/JS/SVG 均已内嵌 HTML，不需要外部字体、CDN、依赖包。
