# Foxy Switcher TUI Design — LURA-Term v1

> 关联文档：[DESIGN_SYSTEM.md](DESIGN_SYSTEM.md)（Web/Tauri 端 LURA v1）、[PRD.md](PRD.md)
> 现存实现：[server/tui/model.go](../server/tui/model.go)
>
> 本文是 TUI 视觉重构的设计稿（不是实现规范）。终端环境与浏览器约束差异巨大（无毛玻璃、无阴影、无任意定位、字符网格、字体宽度固定），因此本文采取 **"LURA 同源、终端原生"** 策略——色板与状态语义跨平台共用，但布局、组件、交互全部按终端原语重新设计。

---

## 0. 综述

LURA-Term 是 Foxy Switcher TUI v0.2 视觉版本的代号。整体语言：

- **基底**：现代 TUI 习惯（k9s / lazygit / btop 一脉），全屏 alt-screen，圆角面板组合，颜色丰富但有节制。
- **个性**：与 Web 端共用 LURA 暖橘主色，账号 active/cooldown/disabled 三态色与 Tauri 完全一致。
- **节奏**：split-pane —— 左 list 右 detail，底部 status bar + key chips。窄终端降级到单列。

设计原则：

1. **状态先于装饰** —— 任何字符都必须先回答"这是什么状态"。冷色仅用于 muted/dim，禁止只用颜色表达状态。
2. **字符即组件** —— 终端没有 div，组件靠 Unicode 字符 + 颜色 + 边框组合而成；要避免依赖 Nerd Font/Powerline。所有图形字符都从 [Unicode block / box-drawing] 标准段挑选。
3. **暖色仅用在身份与品牌**（标题、accent rail、selected 行）；信息状态严格用 ok / warn / danger 三色。
4. **Adaptive 一等公民** —— 全部 token 走 `lipgloss.AdaptiveColor{Light, Dark}`，亮终端不刺眼、暗终端不褪色。
5. **键盘绝对优先** —— 所有功能可达；鼠标支持是 nice-to-have，不假设。

---

## 1. 与 Web 端的关系

| 项 | Web (LURA) | TUI (LURA-Term) | 说明 |
| --- | --- | --- | --- |
| 主色 | `--c-orange-500 #ff7a1a` | `accentBrand` truecolor 同值 | 跨端一致 |
| 状态语义 | ok / warn / danger / muted | 同名 | 阈值也一致：utilization <75 ok / 75-90 warn / ≥90 danger |
| 字体 | -apple-system / SF Pro | 终端默认等宽 | 不强制 Nerd Font |
| 圆角 | `--radius-card 10px` | `lipgloss.RoundedBorder` | 字符近似 |
| 阴影 | `--shadow-card` | 不可用 | 用边框 + 留白替代 |
| 间距 | 4px scale | cell scale (1 cell ≈ 7-8px) | 数字直接用 cell |
| 动效 | 120-280ms 过渡 | 仅 spinner / 5s tick | 终端逐字符重绘，过渡动画意义不大 |
| Mascot | LURA 全身 SVG | ASCII LURA（空状态用） | 见 §7.6 |
| 暗黑 | `prefers-color-scheme` | `lipgloss.HasDarkBackground()` | 自动适配 |

---

## 2. Foundations

### 2.1 调色

#### 2.1.1 Reference 色（与 Web 端共享）

直接复用 [DESIGN_SYSTEM §2.1.1](DESIGN_SYSTEM.md#211-reference-色板) 的 hex，只挑出 TUI 实际会渲染的子集：

```go
// 命名约定：cOrange / cGray / cGreen / cAmber / cRed / cBlue
// 仅在 system token 内部使用，组件代码不直接引用 reference 色。

cOrange300 = "#ffb066"
cOrange400 = "#ff9233"
cOrange500 = "#ff7a1a"  // brand primary
cOrange700 = "#b94a00"

cGray100  = "#f3f3f5"
cGray400  = "#b8b8c0"
cGray500  = "#8e8e96"
cGray600  = "#6e6e76"
cGray800  = "#2a2a2e"
cGray900  = "#1a1a1c"
cGray1000 = "#0d0d0f"

cGreen500 = "#22c55e"
cGreen400 = "#4ade80"
cAmber500 = "#f59e0b"
cAmber400 = "#fbbf24"
cRed500   = "#ef4444"
cRed400   = "#f87171"
cBlue500  = "#3b82f6"
cBlue400  = "#60a5fa"
```

#### 2.1.2 System token（语义层，组件唯一可用）

全部走 `lipgloss.AdaptiveColor{Light, Dark}`：

| Token | Light | Dark | 用途 |
| --- | --- | --- | --- |
| `textPrimary` | `cGray1000` | `#f5f5f7` | 主文字、账号名 |
| `textSecondary` | `cGray600` | `#b0b0b6` | email / org / meta |
| `textTertiary` | `cGray500` | `#8a8a92` | 帮助行、disabled 文字 |
| `textOnAccent` | `#ffffff` | `#ffffff` | accent 底色上的文字 |
| `bgWindow` | terminal default | terminal default | 不绘制 |
| `bgPanel` | `cGray100` | `cGray900` | 面板内底（多数终端用 ANSI 24bit bg；窄/旧终端可禁用） |
| `bgSelected` | `cOrange-50 → ANSI` | `rgba(255,122,26,0.16)` | 选中行 |
| `bgPill` | `rgba(0,0,0,0.05)` | `rgba(255,255,255,0.08)` | plan badge / chip |
| `borderSubtle` | `rgba(0,0,0,0.18)` | `rgba(255,255,255,0.18)` | 面板圆角边框 |
| `borderStrong` | `cGray400` | `cGray600` | section divider |
| `accentBrand` | `cOrange500` | `cOrange400` | 标题 / accent rail / focus |
| `accentSoft` | `cOrange-100` | `rgba(255,122,26,0.16)` | key chip / plan badge bg |
| `ok` | `cGreen500` | `cGreen400` | active 状态点、低利用率 |
| `warn` | `cAmber500` | `cAmber400` | cooldown、中利用率 |
| `danger` | `cRed500` | `cRed400` | 错误、高利用率 |
| `info` | `cBlue500` | `cBlue400` | 信息提示 |
| `muted` | `cGray500` | `cGray500` | disabled 状态点 |

#### 2.1.3 终端能力降级

- **truecolor 终端**（绝大多数现代终端）：用 hex 直出。
- **256 色终端**：lipgloss 自动降级到最近 ANSI 索引。
- **8/16 色终端 / `NO_COLOR=1`**：lipgloss 进一步降级到基本色，状态全靠字符（●/○/▸）+ 加粗/反色表达。设计上**所有状态信息必须有非颜色表达**——状态点字符不同、selected 行有 `▍` 前导。
- **判定**：`lipgloss.ColorProfile()`；遇到 `Ascii` 时强制走 §6.4 的"低能力降级布局"。

### 2.2 字符语汇（替代 Typography）

终端没有字号字重，"层级"靠 **粗细 + 颜色 + 字符前缀** 组合：

| 角色 | 渲染 | 例子 |
| --- | --- | --- |
| Title | Bold + accentBrand | `**foxy-switcher**` |
| H2 / Panel header | Bold + textPrimary + uppercase | `**ACCOUNTS**` |
| Body | textPrimary | 账号名 |
| Caption | textSecondary | email / 组织 |
| Meta | textTertiary | "3m ago" |
| Key cap | accent on accentSoft bg | `[a]` |
| Inline code | textPrimary on bgPill | OAuth URL |

字符建议：

- 数字列宽优先用空格右对齐，例如 `%5s` 保证百分比对齐。
- emoji **不进入 UI 主体**（与 Web §1.4 语气一致）；仅状态字符 ●/○/⚠/✓/✗ 例外。

### 2.3 间距

终端单位是 cell。所有 `.Padding(...)` / `.Margin(...)` 只能取以下值：

| Token | cell |
| --- | --- |
| `space-0` | 0 |
| `space-1` | 1 |
| `space-2` | 2 |
| `space-3` | 3 |
| `space-4` | 4 |

约定：
- 面板 padding 默认 `(0, 1)`（上下 0、左右 1 cell）。
- 行内 gap 用单空格；强分组用 ` · `。
- 面板间垂直留白 1 行。
- 标题与下一行内容间空 1 行。

### 2.4 边框

| Token | lipgloss | 用途 |
| --- | --- | --- |
| `borderRound` | `RoundedBorder()` | Panel / Modal |
| `borderNormal` | `NormalBorder()` | section divider（仅 Top） |
| `borderThick` | `ThickBorder()` | 焦点边框（不推荐使用，太重） |
| `borderHidden` | `HiddenBorder()` | 占位（保持对齐） |

每个 panel 标题嵌入 top border 左 2 cell：

```
╭─ ACCOUNTS ────────────────────╮
│ ...                           │
╰───────────────────────────────╯
```

### 2.5 字符词典

| 用途 | 字符 | 备选 |
| --- | --- | --- |
| Status dot · active | `●` | — |
| Status dot · cooldown | `◐` | `●`（warn 色）|
| Status dot · disabled | `○` | — |
| Status dot · error | `✗` | — |
| Cursor / pointer | `▸` | `›` |
| Accent rail (selected) | `▍` | `┃` |
| Plan badge bracket | `[ ]` | — |
| Key chip bracket | `[ ]` | — |
| Progress block (filled) | `█` | `▰` |
| Progress block (empty) | `░` | `▱` |
| Toast · ok | `✓` | — |
| Toast · err | `✗` | — |
| Toast · info | `ℹ` | — |
| Toast · warn | `⚠` | — |
| Section bullet | `›` | — |
| Spinner | `⣾⣽⣻⢿⡿⣟⣯⣷` | bubbles/spinner.Dot |
| Empty state mascot | 见 §7.6 | — |

所有字符在 BMP 内，Unicode 9.0 以前定义，无 emoji presentation 风险。

---

## 3. Components

### 3.1 StatusDot

```
●  active           ok 色
◐  cooldown 12m30s  warn 色（字符自带"半"语义，无需额外文字）
○  disabled         muted 色
✗  error            danger 色
```

- 渲染：`lipgloss.NewStyle().Foreground(<tone>).Render("●")`
- 永远配字符状态 + 后接文字状态名，不能仅靠颜色。

### 3.2 Pill / Badge

格式 `[ value ]`，外面**没有空格**，内部贴边。

| 类型 | bg | fg | 例 |
| --- | --- | --- | --- |
| Plan | `accentSoft` | `accentBrand` | `[max20]` |
| In-use | `accentBrand` | `textOnAccent` | `[ In use ]` |
| Active count | `bgPill` | `textSecondary` | `[ 5 accts ]` |
| Warn pill | `warn` 透明 / warn 字 | warn fg | `[ refresh due ]` |

实现：`lipgloss.NewStyle().Background(...).Foreground(...).Padding(0, 1)`。

### 3.3 ProgressBar

```
█████████░  92%
████░░░░░░  42%
░░░░░░░░░░   —     // no-data
```

- 默认宽度 10 cell，可压缩到 4（窄终端）。
- 颜色：`utilization < 75 → ok` / `75-90 → warn` / `≥90 → danger`；空槽用 textTertiary。
- 后接百分比，`%4.0f%%` 右对齐 4 cell。
- 无数据时整条 dim + `—`。
- 配套（详情面板）显示 reset 倒计时，格式 `resets in 3h 12m`。

### 3.4 KeyChip

```
[a] add   [r] refresh   [c] cooldown
```

- `[a]` = `accentBrand` fg + `accentSoft` bg + `Padding(0, 0)`，括号本身参与渲染。
- 后接 `add` 用 `textSecondary`，与下一个 chip 间隔 3 空格。
- 整组帮助行用单行水平排版；超出宽度时换行（lipgloss `MaxWidth` 自动）。

### 3.5 Panel

```
╭─ TITLE ──────────────────────────╮
│  内容                            │
│  ...                             │
╰──────────────────────────────────╯
```

- `lipgloss.NewStyle().Border(RoundedBorder()).BorderForeground(borderSubtle).Padding(0, 1)`
- 标题嵌入 top border：`BorderTop(true)` + 自定义渲染（lipgloss 1.x 通过手动拼接 `╭─ TITLE ─...─╮`）。
- 标题色：accentBrand bold。

### 3.6 SelectionAccentRail

选中行最左插入：

```
▍● alice           [max20]   active   ████░░ 42%
```

- `▍` 用 `accentBrand` fg。
- 整行 bg = `bgSelected`。
- 非选中行最左是 1 个空格，保证列对齐。

### 3.7 Spinner

- bubbles/spinner.New(WithSpinner(spinner.Dot))，8fps（默认）。
- 在 statusMsg 行替代 `✓`/`✗` 前缀，文案 `Refreshing…` / `Setting cooldown…` etc.
- 仅 op 进行中显示；完成后切换为 toast 行。

### 3.8 Toast / StatusLine

单行，行首带 leading icon：

```
✓ Token refreshed for alice          // ok
✗ Network error: connection refused  // danger
ℹ Already active — no change         // info
⚠ Cooldown 6h set on bob             // warn
```

- 空闲态显示一个不可见占位行（保持高度稳定）。
- 5s 后自动消退（新增 timer，model.statusMsg 同步清空）。

### 3.9 EmptyState

```
            ╱╲      ╱╲
           ( o )    ( o )
            ───   LURA
        no accounts in the pool yet

        press [a] to add your first
```

- 居中（`lipgloss.Place(width, height, Center, Center, ...)`）。
- ASCII Mascot 见 §7.6；后续可换 figlet 或更精致的字符画。
- 主标 textPrimary，副标 textSecondary，CTA 用 keychip。

### 3.10 ConfirmModal（小卡片）

```
       ╭─ Delete account ──────────────────╮
       │                                   │
       │  Permanently remove alice         │
       │  (alice@example.com)?             │
       │                                   │
       │  refresh_token will be discarded. │
       │                                   │
       │  [y] confirm   [n] cancel         │
       │                                   │
       ╰───────────────────────────────────╯
```

- 居中，max-width 50 cell。
- 底色仍是 bgPanel；不实现 backdrop 遮罩（终端不可行）。

---

## 4. Layout

### 4.1 Page Skeleton（≥120 列）

```
╭─ foxy-switcher ─ managing acct #3 ─ 5 accounts ─ refreshed 4s ago ─────────────────╮
│                                                                                     │
│  ╭─ ACCOUNTS ──────────────────────╮  ╭─ alice ───────────────────────────────╮    │
│  │                                  │  │                                       │    │
│  │ ▍● alice            [max20]      │  │ alice@example.com                     │    │
│  │   ○ bob             [max5]       │  │ Acme Corp                             │    │
│  │   ● charlie         [max20]      │  │                                       │    │
│  │   ◐ dan             [max5]       │  │ ╭─ Usage ─────────────────────────╮  │    │
│  │   ✗ eve             [max20]      │  │ │ 5h        ████████░░  42%       │  │    │
│  │                                  │  │ │ 7d Opus   ██░░░░░░░░  18%       │  │    │
│  │                                  │  │ │ 7d Sonnet ███░░░░░░░  29%       │  │    │
│  │                                  │  │ ╰─────────────────────────────────╯  │    │
│  │                                  │  │                                       │    │
│  │                                  │  │ Status      ● active                  │    │
│  │                                  │  │ Last used   3m ago                    │    │
│  │                                  │  │ Token       expires in 47m            │    │
│  │                                  │  │ Updated     12s ago                   │    │
│  │                                  │  │                                       │    │
│  ╰──────────────────────────────────╯  ╰───────────────────────────────────────╯    │
│                                                                                     │
├─────────────────────────────────────────────────────────────────────────────────────┤
│  ✓ Token refreshed for alice                                                        │
├─────────────────────────────────────────────────────────────────────────────────────┤
│  [a] add   [r] refresh   [e] enable   [d] disable   [c] cooldown   [x] del   [q]    │
╰─────────────────────────────────────────────────────────────────────────────────────╯
```

四个区：

1. **Header bar**（顶部边框嵌入）：app 名 + cred 状态 + 账号计数 + 上次刷新。
2. **Body**（左右两 panel）：左 35% 宽 ACCOUNTS list，右 65% DETAIL。
3. **Status line**（中间 divider 隔出的 1 行）：toast / spinner。
4. **Footer**（底部边框上方 1 行）：key chips。

外层用一个 `RoundedBorder` 大框包整个 alt-screen，内部用 `JoinVertical` 堆叠四块；body 内 `JoinHorizontal` 拼 list + detail。

### 4.2 modeList 区域细节

#### 4.2.1 ACCOUNTS panel（左）

每行结构：

```
▍● alice            [max20]
```

- col 1: `▍` 或空格（accent rail）
- col 2: status dot + 1 空格
- col 3: 账号名（左对齐，截断到 panel 宽度 - 14）
- col 末: plan badge（右对齐）

不展示 STATUS / 5H / 7D —— 那些信息全部移到右侧 detail panel。这样 list 极简，sweep 视线只看名字+plan。

cooldown 时 status dot 字符变 `◐` warn 色，名字后追加 `(12m)` dim 文字。

#### 4.2.2 DETAIL panel（右）

只展示当前光标账号；切换光标时 detail 区即时跟随。组成：

1. Title bar：账号名（panel header）。
2. Owner 区：full_name / email / org（dim）。
3. Usage 子 panel（嵌套）：3 条 ProgressBar + reset 倒计时。
4. Meta dl：Status / Last used / Token expires / Updated。

无选中（空账号池）时整个 detail panel 渲染 §3.9 的 EmptyState 字符画。

#### 4.2.3 Status line

```
✓ Token refreshed for alice
```

或在 op 进行中：

```
⣾ Refreshing alice…
```

或空闲：渲染单空格保持高度稳定。

#### 4.2.4 Footer key chips

```
[↑↓] move   [a] add   [r] refresh   [e] enable   [d] disable   [c] cooldown   [x] delete   [g/G] top/bot   [R] reload   [q] quit
```

宽度不够时按优先级折行：a / r / c / x 始终同行，e / d / g / G / R 折到第二行，q 永远末尾。

### 4.3 modeAddPaste

```
╭─ Add account ─────────────────────────────────────────────────────────────────╮
│                                                                                │
│  Step 1   Open this URL in a browser and approve the OAuth request:           │
│                                                                                │
│           ╭──────────────────────────────────────────────────────────────╮   │
│           │ https://claude.ai/oauth/authorize?client_id=...&state=abc123 │   │
│           ╰──────────────────────────────────────────────────────────────╯   │
│           [c] copy                                                             │
│                                                                                │
│  Step 2   Paste the resulting code (format: code#state):                      │
│                                                                                │
│           › █                                                                  │
│                                                                                │
├────────────────────────────────────────────────────────────────────────────────┤
│  [enter] submit   [esc] cancel                                                 │
╰────────────────────────────────────────────────────────────────────────────────╯
```

亮点：

- 步骤号用 textSecondary 的 `Step 1` / `Step 2` 标签（不是数字泡）。
- URL 嵌套 panel + dim 边框。
- 新增 `c` 键：把 URL 复制到系统剪贴板（`golang.design/x/clipboard` 或 `tea.Cmd` 调用 OS API），节省"手动选中拖蓝再 ⌘C"。
- 输入 prompt `›` 用 accentBrand。

### 4.4 modeCooldown

```
╭─ Cooldown · alice ────────────────────────────────────────────────────────────╮
│                                                                                │
│   ▸ Clear cooldown                                                             │
│     5 minutes                                                                  │
│     30 minutes                                                                 │
│     1 hour                                                                     │
│     6 hours                                                                    │
│                                                                                │
├────────────────────────────────────────────────────────────────────────────────┤
│  [↑↓] pick   [enter] apply   [esc] cancel                                      │
╰────────────────────────────────────────────────────────────────────────────────╯
```

- 居中 panel，max-width 60 cell，max-height 14 行。
- 当前选项：`▸` accent fg + 整行 bgSelected。
- 其余项前置 2 空格保持对齐。

### 4.5 modeConfirmDelete

见 §3.10。

### 4.6 modeError（fatal）

```
                       ╭─ Something went wrong ─────────────╮
                       │                                    │
                       │  ✗ daemon unreachable              │
                       │                                    │
                       │  dial tcp 127.0.0.1:7437:           │
                       │  connect: connection refused        │
                       │                                    │
                       │  [any key] return                  │
                       │                                    │
                       ╰────────────────────────────────────╯
```

- 居中。
- danger 配色，但**不全屏红**——边框 danger，标题 danger，内容仍 textPrimary。

---

## 5. Responsive

`m.width` 触发的三档布局：

| 宽度 | 名字 | 布局 |
| --- | --- | --- |
| ≥120 cell | "wide" | §4.1 split-pane，detail 跟随光标 |
| 80–119 cell | "regular" | 单列 list；选中行下方插入 inline detail（usage bars + meta），其他账号收起 |
| <80 cell | "narrow" | 单列 list 仅 `▍● name [plan]`；usage / meta 完全省略；detail 改成按 `?` 弹小 modal |

实现策略：在 `Update` 处理 `tea.WindowSizeMsg` 时计算 `m.layout = "wide"|"regular"|"narrow"`，View 路由器据此分发。

子屏（add / cooldown / confirm）都使用 `lipgloss.Place` 居中，自动适配窗口大小；最小窗口 60×16 以下显示 fallback 文案"window too small"。

---

## 6. Motion / Refresh

| 事件 | 行为 |
| --- | --- |
| 启动 | 立即 refreshCmd，渲染 spinner-on-empty 直到首批数据回来 |
| 5s tick | 静默重渲染；不闪烁（lipgloss 同字符不重写）|
| `r` 单账号 refresh | statusLine 切换为 spinner + "Refreshing alice…"；完成后切 toast `✓ Token refreshed for alice`；toast 5s 后清空 |
| 光标切换 | detail panel 即时切换；无过渡动画 |
| 选中变化 | 选中 bg / accent rail 立即切换 |
| `R` 全量 reload | 上方 cred 状态闪一次 spinner（在 header 中，2s 内回归） |

`prefers-reduced-motion` 在终端无对应 API；spinner 默认开启，可通过 env `FOXY_TUI_NO_SPINNER=1` 关闭。

---

## 7. 杂项

### 7.1 Clipboard

`modeAddPaste` 的 Step 1 加 `[c] copy` 把 URL 写入系统剪贴板。Bubble Tea 0.27+ 自带 `tea.SetClipboard` 命令；fallback 用 `golang.design/x/clipboard`。

### 7.2 Mouse

Bubble Tea `tea.WithMouseCellMotion()`：
- 滚轮：上下移动光标（list panel）。
- 点击账号行：等价于 `enter`（暂无 enter 行为，预留）。
- 点击 key chip：触发对应键盘动作。

鼠标支持是 nice-to-have，所有功能必须键盘可达。

### 7.3 Search / Filter（v0.3 预留）

按 `/` 进入搜索模式：list panel header 替换为 `╭─ ACCOUNTS · /alice█ ──...─╮`，account list 实时过滤。当前版本不做。

### 7.4 Help overlay（v0.3 预留）

按 `?` 弹出 modal 显示完整快捷键表。当前版本帮助行长期可见，不做 overlay。

### 7.5 配色文件

`~/.foxy-switcher/tui-theme.json` 可覆盖 §2.1.2 的 system token。当前版本不做，但所有颜色都集中在一个 `theme.go` 里方便后续接入。

### 7.6 ASCII Mascot

LURA 简笔字符画（用于 EmptyState）：

```
       ╱╲    ╱╲
      ╱  ╲__╱  ╲
     ╱    ●●    ╲
    │   ╲ ◡  ╱   │
    ╲    ╲__╱    ╱
     ╲___    ___╱
         │ │
        ╱   ╲
```

不强求与 Web 的 Mascot SVG 视觉一致，但保留橘色填充语义（前景 accentBrand）。

---

## 8. 实现拆解

为了把改造分成可独立 ship 的小步，建议按以下 phase 落地。每个 phase 都能独立验证。

### Phase 1 — Theme & Tokens（1 天）

- 新建 `server/tui/theme.go`，把 §2.1.2 全部 system token 定义为 `lipgloss.AdaptiveColor` + 命名导出。
- 替换 `model.go:402-413` 的 7 个 style 变量为 token-based 引用。
- 视觉变化：颜色变橘色系，亮/暗终端自适配；其它布局不变。
- 验收：在 truecolor、256 色、`NO_COLOR=1` 三档终端各跑一次，状态字符全可读。

### Phase 2 — Components（1.5 天）

- `components.go` 新文件，实现 §3.1 / 3.2 / 3.3 / 3.4 / 3.6 / 3.7 / 3.8。
- 每个组件配 `_test.go`，渲染一次比对预期字符串（snapshot）。
- 视觉变化：仅在被组件替换的位置出现（暂未替换）。

### Phase 3 — modeList split-pane（2 天）

- 重写 `viewList`，按 §4.1 + §4.2 拆出 header / list panel / detail panel / status line / footer。
- 实现 §5 的三档响应式布局。
- 视觉变化：主屏改头换面。
- 验收：80 / 100 / 140 列各跑一次，光标切换 detail 正确跟随，selected accent rail 出现在左侧。

### Phase 4 — modeAddPaste / Cooldown / Confirm / Error（1 天）

- 按 §4.3-4.6 重写其余四个 view 函数。
- modeAddPaste 加 clipboard `[c]` 键。

### Phase 5 — Empty state + ASCII Mascot + Spinner（0.5 天）

- 列表为空时渲染 §3.9。
- bubbles/spinner 接入 statusLine。
- toast 自动消退 timer（新增 `tea.Tick` 5s 后清 statusMsg）。

### Phase 6 — Mouse + Help overlay (v0.3 deferred)

不在本期。

### 累计工期

约 6 个工作日，单人节奏。

---

## 9. 迁移映射

现 [server/tui/model.go](../server/tui/model.go) → 新结构对照：

| 现位置 | 现内容 | 新位置 |
| --- | --- | --- |
| `model.go:402-413` | 7 个 lipgloss 内联 style | `theme.go` token 表 |
| `model.go:415-471` `viewList` | 单表格 list | `view_list.go`（拆 header/list/detail/status/footer）|
| `model.go:473-487` `viewAddPaste` | 两步骤纯文本 | `view_add_paste.go`（panel + clipboard 键）|
| `model.go:489-506` `viewCooldown` | 5 项预设 | `view_cooldown.go`（居中 panel）|
| `model.go:508-517` `viewConfirmDelete` | 一句提示 | `view_confirm_delete.go`（modal panel）|
| `model.go:523-559` `formatRow / pctOrDash / statusFor` | 表格字符串组装 | 解散到 §3 components |
| `model.go:561-580` `humanMillis / humanAge` | 时间格式化 | 保留，搬到 `format.go` |

`model` 结构体新增字段（不删旧）：

```go
type model struct {
    // ...原有字段
    layout layoutMode  // wide | regular | narrow，由 WindowSizeMsg 推算
    spinner spinner.Model
    pendingOp string   // spinner 标签，op 进行中显示
    statusExpiresAt time.Time  // toast 自动消退时间
}
```

---

## 10. 待对齐项

| # | 问题 | 当前默认 |
| --- | --- | --- |
| 1 | Mouse 支持要不要进 v0.2？ | 否，v0.3 |
| 2 | `~/.foxy-switcher/tui-theme.json` 自定义主题要不要做？ | 否，v0.3 |
| 3 | `[c] copy` 在 add-paste 屏调系统剪贴板，依赖 `golang.design/x/clipboard`，要新增 dep —— OK 吗？ | 倾向 OK，但可降级为空操作（提示用户用终端选中复制） |
| 4 | ASCII Mascot 的具体笔画是否要重新设计师出图？ | 当前版本占位即可，v0.3 升级 |
| 5 | toast 5s 自动消退，会不会让用户错过重要错误？ | 错误（statusErr）不消退，仅 ok 消退 |
| 6 | 窄终端 (<80 cell) 的"按 `?` 弹 detail"要不要做？ | 否，仅显示精简列；按 `?` 在 v0.3 是 help overlay |
