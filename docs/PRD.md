# Foxy Switcher — 桌面端 PRD（v0.2 / LURA 设计落地）

> 文档配套设计稿：[docs/design.png](design.png)
> 关联现有实现：[src/App.tsx](../src/App.tsx)、[src/styles.css](../src/styles.css)、[server/](../server/)
> 设计系统规范：[docs/DESIGN_SYSTEM.md](DESIGN_SYSTEM.md)

## 0. 文档目的

将设计师交付的 LURA 视觉版本翻译成可被工程拆解的需求清单。每一节都以"可观察、可验收"为目标——读者读完应该知道**要做什么**、**何时算完成**、**与现有 v0.1 的差异**。

本文不重新论证产品价值（见 [README.md](../README.md)），只描述 v0.2 这版界面要把哪些行为做出来。

## 1. 一句话定位

Foxy Switcher 是一个常驻在本机的 Claude 订阅账号池：用户把多个订阅一次性登记进来，应用按 LRU + 冷却策略自动把"最该用的"那个账号注入 Claude Code 的钥匙串；当所有账号都用爆时，干净地把用户原生登录还原回去。

桌面端 GUI 的工作是把"池子里现在发生什么"用一眼看懂的方式呈现，并允许用户在自动策略不够用时手动接管。

## 2. 目标用户与典型场景

| 用户画像 | 核心诉求 |
| --- | --- |
| 重度 Claude Code 用户，持有 2–5 个 Claude 订阅 | 不想再手动 logout/login；要看到"哪一个账号还能打多久" |
| 工程师同时管理个人/团队订阅 | 关心组织归属、订阅类型、冷却剩余 |
| 小型团队的"账号管家" | 要看 Activity Log 推断哪一段时间池子异常 |

典型场景：

1. **首次安装** — 用户启动 App，看到空 Dashboard 与"Add account"主 CTA。
2. **日常使用** — 用户偶尔打开 App 看一眼总览，确认 Auto Switch 处于开启、当前注入的是哪个账号、谁马上要冷却。
3. **手动接管** — Auto Switch 关闭，用户在 Accounts 页对某个账号点 "Use now"，应用立刻把那个账号注入到钥匙串。
4. **疑难排查** — 用户去 Activity Log 看 credinject / refresh / usage poll 的时间线和失败原因。
5. **设置调整** — 用户在 Settings 改主题、修改自动启动、清理本地数据。

## 3. 信息架构

```
Foxy Switcher
├── Dashboard          ← 默认着陆页，全局 Overview
├── Accounts           ← 账号池 CRUD + 详情
│   └── Account detail (右侧面板 / 抽屉)
├── Activity           ← 时间线日志
└── Settings
    ├── General
    ├── Appearance
    ├── Behavior
    └── About
```

桌面端使用**左侧 Sidebar + 主内容区**的二栏布局。Sidebar 始终可见，宽度固定 220px（折叠态 64px，仅图标）。

## 4. 全局元素

### 4.1 顶栏（Topbar）

- 永远显示：当前页面标题 + 当前注入账号摘要（"Managing {account.name}" / "Idle"）。
- 右上：**Auto Switch 总开关**（Toggle）+ 通知小铃铛（v0.2 占位）+ 用户头像。
- Auto Switch 关闭时：顶栏在标题旁出现一个橙色 dot + "Manual" pill，提醒用户自动策略已被覆盖。

### 4.2 Sidebar

- Logo + 产品名 "Foxy Switcher"，点击回 Dashboard。
- 主导航：Dashboard / Accounts / Activity / Settings，每项 1 个图标 + 1 个文字。
- 底部：守护进程状态指示器（绿 = 健康 / 橙 = 重连中 / 红 = 不可达），点击展开端口与版本号 popover。
- 折叠按钮放在底部，状态保存到 localStorage（`fx.sidebar.collapsed`）。

### 4.3 全局快捷键

| 快捷键 | 行为 |
| --- | --- |
| `⌘N` / `Ctrl+N` | 触发 Add Account |
| `⌘,` / `Ctrl+,` | 跳到 Settings |
| `⌘1`–`⌘4` | 切换 Sidebar 四个一级页面 |
| `⌘R` / `Ctrl+R` | 主动 refetch（与轮询独立） |
| `Esc` | 关闭当前 Sheet / Drawer / Kebab |

### 4.4 守护进程感知

GUI 每 5s 轮询 `/healthz` + `/api/cred/status`。失联 10s 后顶栏出现红色 banner："Daemon unreachable — retrying."，按钮 "Restart sidecar"（调用 Tauri 命令重启 sidecar）。

## 5. 页面规格

### 5.1 Dashboard

**目的**：让用户开 App 后 3 秒内回答"我的池子现在是什么状态？"

**布局（自上而下）**：

1. **欢迎条** — `Howdy, {first_name}` + 当前时间段问候（早上 / 下午 / 晚上）。
2. **Status Card 行** — 4 张并排 KPI 卡片：
   - **In use** — 当前注入账号名 + plan + 剩余 token。空态："No account injected"。
   - **Pool size** — `{active}/{total}` accounts，副文案 "X disabled, Y in cooldown"。
   - **Peak utilization** — 池中最高 utilization 百分比 + 来自哪个账号；颜色随 0–74 / 75–89 / 90+ 切换。
   - **Next cooldown ends** — 最近一个即将解冻的账号 + 剩余时间。
3. **Auto Switch 卡片** — 大 Toggle + 三选一策略：`LRU`（默认）/ `Lowest utilization` / `Round-robin`。Toggle 关闭时，卡片下方出现一行说明："Manual mode — selection only changes when you click Use now."
4. **Account list（精简）** — 显示池里所有账号的紧凑视图（每行 1 行，不可展开），最多 5 行，溢出 "View all → /accounts"。每行：状态点 / 名字 / plan pill / utilization% / 下次冷却倒计时 / kebab。
5. **Recent Activity** — Activity Log 的最近 5 条，时间 + 事件 + 账号。"View all → /activity"。
6. **Usage trend chart** —（可选，v0.2 阶段允许仅画占位）池子总 utilization 24 小时折线图，分 5h / 7d / 7d-Sonnet 三条线。

**响应式**：

- ≤ 1280px：4 张 KPI 卡片自动 2×2。
- ≤ 768px：Sidebar 折叠为底部 TabBar，KPI 卡片单列堆叠。

**验收**：

- [ ] 无账号时所有 KPI 卡片都有合理空态文案（不是"—"或"NaN"）。
- [ ] Auto Switch toggle 立即调用 `POST /api/auto-switch`（新接口，见 §7），并在 5s 内反映 cred status 变化。
- [ ] Recent Activity 在 daemon 离线期间显示 stale 提示。

### 5.2 Accounts

**目的**：账号 CRUD + 单账号深度信息。

**布局**：

- 左：占满主内容区的 list（沿用现有 `.list .row` 设计），每行可点击展开成内嵌详情（沿用现有 v0.1 行为）。
- 右：占据 ~40% 宽度的**详情 Drawer**（v0.2 新增），固定显示当前选中账号的完整信息。点击空白处或 ESC 收起。
- 顶部 toolbar：搜索框（按 name / email 模糊匹配） + 状态筛选（All / Active / Disabled / In cooldown） + "Add account" 主按钮。

**Account row 字段**：

| 区 | 内容 |
| --- | --- |
| status dot | ok / warn / danger / muted |
| primary | name + plan pill + ("In use" pill 当前注入) |
| secondary | full_name · email · organization_name |
| trailing | peak utilization% / next-cooldown countdown |
| kebab | Use now / Refresh now / Disable / Delete |

**Drawer 字段**：

- 头部：avatar（首字母色块） + name + plan + 状态标签
- 三个 Usage Bar：5h / 7d Opus / 7d Sonnet
- Meta 网格：Status / Last used / Token expires / Usage updated / Created / Organization UUID
- Action group：Use now（主） / Refresh now / Disable·Enable / Delete（destructive）

**Add account 流程**：保留现有 PKCE 二步流程（拷贝 URL → 粘贴 `code#state`），但从 inline sheet 升级为**居中 Modal**，关闭时确认未提交内容。

**验收**：

- [ ] 搜索过滤是纯客户端，对 100 个账号无明显延迟。
- [ ] 当前注入账号（`managed_account_id`）始终显示 "In use" pill 且默认选中并展开 drawer。
- [ ] 删除当前注入账号时，前端立刻乐观更新到 "Idle"，并由下一次轮询拉取真实 cred status 修正。
- [ ] Disable 当前注入账号会触发后端切换；前端显示 "Switching..." 直到 cred status 更新。

### 5.3 Activity Log

**目的**：让用户看到 daemon 在背后做了什么——为什么切换、何时刷新、哪个 API 失败。

**事件来源**（v0.2 新增 `GET /api/activity`，由 daemon 维护内存 + SQLite ring buffer，最多 1000 条）：

| 事件类型 | 触发 |
| --- | --- |
| `account.added` / `account.deleted` | CRUD |
| `account.disabled` / `account.enabled` | 用户操作 |
| `cred.injected` / `cred.restored` | credinject coordinator |
| `token.refreshed` | refresh.Scheduler |
| `usage.polled` | refresh.UsagePoller |
| `cooldown.entered` / `cooldown.cleared` | selector / 用户手动 |
| `daemon.started` / `daemon.stopped` | 进程生命周期 |
| `error.*` | 任意失败带错误信息 |

**布局**：

- 顶部 filter chips：All / Switches / Refreshes / Errors。
- 时间线列表：每条 = 时间戳 + 事件类型图标 + 一句话描述 + 涉及账号链接。Error 类整行红色背景。
- 右侧（可选）：选中某条事件后展开 raw payload（JSON）便于诊断。

**响应式**：≤ 768px 直接单列时间线，filter chips 横向滚动。

**验收**：

- [ ] 时间显示相对时间（"3 min ago"）+ hover/long-press 看绝对时间。
- [ ] 列表虚拟滚动，10000 条无卡顿。
- [ ] 顶部支持"Pause auto-scroll"（接收新事件时不强制跳到顶部）。

### 5.4 Settings

**General**

- Launch at login（macOS LaunchAgent / Windows Run key / Linux autostart）
- Start minimized in tray
- Data directory（只读显示路径 + "Reveal in Finder" 按钮）

**Appearance**

- Theme：System / Light / Dark
- Sidebar：Always expanded / Auto-collapse on narrow window

**Behavior**

- Auto Switch policy（与 Dashboard 同步）：LRU / Lowest utilization / Round-robin
- Cooldown threshold：utilization 多少%自动入冷却（默认 95）
- Refresh interval：usage 轮询间隔（默认 60s，范围 30–300s）
- Restore native credentials on quit（默认 on）

**About**

- Logo + 版本 + 构建号
- Daemon health 详情（端口、PID、build commit、SQLite 路径）
- Links：Homepage / GitHub / Report issue
- "Reset all data" — 二次确认后清空 state.db 并重启 sidecar

**验收**：

- [ ] 所有 Settings 改动**立刻生效**（不需要 Save 按钮），通过 `POST /api/settings` 持久化。
- [ ] Reset all data 在执行前要求输入 "RESET" 字符串确认。

## 6. 状态机

### 6.1 账号状态（与 v0.1 保持一致）

```
active ──disable──▶ disabled
   │                    │
   │◀────enable─────────┘
   │
   ├──cooldown_until > now──▶ (in cooldown)──auto clear──▶ active
   │
   └──delete──▶ (gone)
```

UI 映射：

| 后端状态 | UI tone | row-status dot | pill 文案 |
| --- | --- | --- | --- |
| active, cooldown_until ≤ now, util < 75 | ok | green | "Active" |
| active, util ≥ 75 | warn | orange | "Heavy use" |
| active, util ≥ 90 | danger | red | "Near limit" |
| active, cooldown_until > now | warn | orange | "Cooldown {time}" |
| active, expires_at − now < 5min | warn | orange | "Refresh due" |
| status != "active" | muted | gray | "Disabled" |
| 当前注入 | 任意 + "In use" pill | — | — |

### 6.2 凭据注入状态（cred status）

```
no-account ──pick──▶ injecting ──ok──▶ injected({id})
                          │              │
                          └──fail────────┤
                                         │
                       on-quit / no-available
                                         │
                                         ▼
                                   restoring-native
                                         │
                                         ▼
                                   native-restored
```

顶栏摘要文案：

| state | 文案 |
| --- | --- |
| `injected` | "Managing {name}" |
| `injecting` | "Switching to {name}…" |
| `restoring-native` | "Restoring native login…" |
| `native-restored` / `no-account` | "Idle" |
| `failed` | "Inject failed — Retry" |

## 7. 数据来源 & API 增量

沿用 [server/httpapi/routes.go](../server/httpapi/routes.go) 已有接口（见 README §HTTP API）。

**v0.2 新增**：

| Method + Path | Body / Query | 用途 |
| --- | --- | --- |
| `GET /api/dashboard` | — | 一次性返回 Dashboard 渲染所需 KPI（聚合后服务端算好，避免前端二次组合） |
| `GET /api/activity?limit=&since=&type=` | — | 时间线分页 |
| `POST /api/auto-switch` | `{ enabled: bool, policy?: "lru"\|"lowest"\|"rr" }` | 改 Auto Switch |
| `GET /api/settings` / `PUT /api/settings` | settings JSON | 持久化用户偏好（存到 SQLite `kv` 表） |

前端轮询节奏：

| 资源 | 节奏 |
| --- | --- |
| `/api/dashboard` | 5s 一次（Dashboard 当前 visible 时） |
| `/api/accounts` | 5s（Accounts 页面） |
| `/api/cred/status` | 2s（Topbar 永久） |
| `/api/activity` | 长连接 SSE 优先，降级到 3s 轮询 |
| `/healthz` | 5s |

## 8. 响应式断点

| 断点 | 触发 | 变化 |
| --- | --- | --- |
| ≥ 1280px | 大屏 | Sidebar 220 + 主区 + Drawer 同时展示 |
| 1024–1279px | 中屏 | Sidebar 220 + 主区；Drawer 改为 overlay 抽屉 |
| 768–1023px | 小屏 | Sidebar 折叠到 64（仅图标）；Drawer 全屏 modal |
| < 768px | 移动端 | Sidebar 退化为底部 TabBar（4 tab）；KPI 单列；Activity 时间线占满 |

注：移动端是当前 Tauri 桌面端的同一份代码在小窗口下的形态，**不是**独立的原生 App。窗口最小尺寸 360×600。

## 9. 非快乐路径

| 场景 | 表现 |
| --- | --- |
| 守护进程未启动 | 全屏 banner "Daemon not running" + "Restart sidecar" 按钮（调用 Tauri 命令） |
| OAuth 网络失败 | Modal 内联 banner，保留用户已粘贴内容 |
| 单接口 5xx | Section 顶部红色 banner，可 Dismiss；不阻塞其他 section |
| 数据 stale（轮询失败 ≥ 30s） | 受影响 section 右上角小 dot："Last updated 1m ago" |
| 池子全冷却 | Dashboard 顶部出现 banner "All accounts cooling — falling back to native login" |
| 浏览器中粘贴 `code#state` 格式错误 | Modal 内 inline error，输入框红色边框 |

## 10. 国际化

v0.2 仍只交付 **英文 UI**。但所有可见字符串通过 `t("namespace.key")` 抽离到 `src/i18n/en.json`，方便后续加 zh-CN。

## 11. 验收标准（Definition of Done）

- [ ] 所有页面在 macOS 14+ / Windows 11 / Ubuntu 22.04 的 Tauri 2 webview 下视觉与设计稿一致。
- [ ] Light / Dark / System 三个主题都正常切换（含图标资源）。
- [ ] 全部交互元素键盘可达（Tab 顺序、Esc 关闭、Enter 提交）。
- [ ] axe-core 静态扫描无 critical / serious 违规。
- [ ] `pnpm build` + `cargo build --release` 双侧无 warning。
- [ ] 与 daemon 失联超过 10s 一定能恢复 UI（不卡死）。
- [ ] 端到端 happy path：启动 → 加 1 个账号 → 看到 In use → 删账号 → 看到 native restored。

## 12. 不在本次范围

- 团队/多用户共享池
- 远程 daemon（仍只支持 127.0.0.1）
- 中文等其他语言 UI（仅做 i18n 抽离）
- iOS / Android 原生 App
- 账号统计的历史趋势导出
- 桌面通知（v0.3 再做）

## 13. 已知不确定点（待与设计师对齐）

> 设计稿分辨率有限，下面这些点是我读图时的最佳推断；若与设计师本意不符，以下文档（特别是 DESIGN_SYSTEM.md 的 token 表）需要再修订。

1. 中间一行第二个子页 "Persona Score" 我未能从图上读清，本文按"Account 详情 / 单账号分析"语义实现；如果设计师指的是池子整体健康分（如 Apple Watch 的 Activity Ring 形态），需要补一个独立 widget。
2. Dashboard 的 Usage trend chart 我把它放进了"可选"——设计稿能看到下方有面积图占位但读不出图例细节。
3. 移动端究竟是 Tauri 自适应还是未来要做 PWA / 原生 App，PRD 当前按"Tauri 同代码响应式"实现；若另有部署形态需要单独立项。
