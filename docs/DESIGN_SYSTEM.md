# Foxy Switcher Design System — LURA v1

> 设计稿来源：[docs/design.png](design.png)
> 关联 PRD：[docs/PRD.md](PRD.md)
> 现存实现：[src/styles.css](../src/styles.css)
>
> 本文档采用 **token-first** 写法：每一节都给出 CSS variable 命名 + 值（Light / Dark）+ 用法。所有 token 都跟现有 `src/styles.css` 命名空间保持一致或向前兼容，工程师可以把这份文档当作直接落 CSS 的清单。

---

## 0. 综述

LURA 是 Foxy Switcher v0.2 视觉版本的代号。整体语言：

- **基底**：Apple Human Interface 风格（系统字体、半透明分隔线、卡片化分组、毛玻璃可选）。
- **个性**：橘色 LURA 狐狸吉祥物 + 暖橘主色 + 圆润几何图标。
- **节奏**：信息密度向 Notion / Linear 方向走（左 Sidebar + 主内容 + 右 Drawer），但保留 macOS 设置面板的"插入式分组列表"。

设计原则：

1. **状态先于装饰** — 任何视觉元素先回答"这是什么状态"再回答"它好不好看"。
2. **可读 > 漂亮** — 除主按钮外避免渐变与阴影；用 0.5px 分隔线代替框线。
3. **暖色仅用在身份与品牌**（logo、主按钮、active 选中）；信息状态严格用 ok / warn / danger 三色。
4. **Dark mode 一等公民** — 每一个 token 都有显式的 dark 取值，不是反相。

---

## 1. 品牌

### 1.1 吉祥物 — LURA / 璐莱

LURA 是一只橘色短毛狐狸，圆头、橙白渐变、眼睛是简笔黑点。它代表"在你身后悄悄帮你切账号的小伙伴"。

| 形态 | 用途 |
| --- | --- |
| **Mark**（仅头像） | App icon / Sidebar logo / Topbar logo / favicon |
| **Mascot**（全身） | 空状态插画 / 启动画面 / 错误页 |
| **Sticker**（表情包变体） | 通知 / Easter egg / 营销页 |

净空：Mark 周围至少留 ¼ Mark 高度的留白；不允许置于纯白以外的复杂背景上。
最小尺寸：Mark 16px（仅在 favicon / Topbar），Mascot 96px。

### 1.2 命名

- 中文展示：**Foxy Switcher**（不译，避免使用"狐狸切换器"）。
- 英文展示：**Foxy Switcher**。
- 内部代号：**LURA**（仅用于设计版本号，UI 中不出现）。
- App bundle ID：`io.foxy.switcher`（与 Tauri 配置一致）。

### 1.3 App 图标矩阵

| 平台 | 形状 | 文件 | 关键约束 |
| --- | --- | --- | --- |
| macOS | 超椭圆（Big Sur shape） | `src-tauri/icons/icon.icns` 含 16/32/128/256/512/1024 + @2x | LURA 居中，画布两侧 ~10% 留白；底部有 1px 内阴影 |
| iOS / iPadOS | 圆角方形（系统裁切） | `icon-1024.png` | 不要自带圆角（系统会裁），LURA 占 64% |
| Android Adaptive | foreground + background 双层 | `ic_launcher_foreground.svg` + `bg.png` | foreground 占安全区 66%，background 用 `--brand-orange-300` 实色 |
| Windows | 多分辨率 ICO | `src-tauri/icons/icon.ico` 16/32/48/256 | 1px 描边深橘以防浅色任务栏吞掉边缘 |
| Web favicon | SVG + PNG fallback | `public/favicon.svg`、`public/favicon-32.png` | 16px 时简化到只剩头部轮廓 |

### 1.4 语气（Voice）

- 亲切但不卖萌：可以说 "Howdy, Yuheng"，不要说 "Hi there friend! 🦊"。
- 错误信息要可执行：不要 "Oops!"，要 "Daemon unreachable — retrying."
- 表情符号仅在 Easter egg / changelog 出现，UI 主体禁止。

---

## 2. Foundations

### 2.1 颜色

颜色全部以 CSS variable 暴露，命名分两层：

- **Reference token**（原色板）：`--c-orange-500` 这种，**不直接用在组件 CSS 里**。
- **System token**（语义）：`--accent`、`--bg-card` 这种，组件只用 system token。

#### 2.1.1 Reference 色板

```css
:root {
  /* Brand orange — LURA */
  --c-orange-50:  #fff7ed;
  --c-orange-100: #ffe8d1;
  --c-orange-200: #ffd0a3;
  --c-orange-300: #ffb066;
  --c-orange-400: #ff9233;
  --c-orange-500: #ff7a1a;  /* primary brand */
  --c-orange-600: #e55f00;  /* hover */
  --c-orange-700: #b94a00;  /* pressed / on-light text */
  --c-orange-800: #883600;
  --c-orange-900: #5a2400;

  /* Neutrals (warm-tinted gray) */
  --c-gray-0:   #ffffff;
  --c-gray-50:  #fafafb;
  --c-gray-100: #f3f3f5;
  --c-gray-200: #e8e8ec;
  --c-gray-300: #d8d8de;
  --c-gray-400: #b8b8c0;
  --c-gray-500: #8e8e96;
  --c-gray-600: #6e6e76;
  --c-gray-700: #4a4a52;
  --c-gray-800: #2a2a2e;
  --c-gray-900: #1a1a1c;
  --c-gray-1000:#0d0d0f;

  /* Semantic — success / warning / danger / info */
  --c-green-400: #4ade80;
  --c-green-500: #22c55e;
  --c-green-600: #16a34a;

  --c-amber-400: #fbbf24;
  --c-amber-500: #f59e0b;
  --c-amber-600: #b45309;

  --c-red-400:   #f87171;
  --c-red-500:   #ef4444;
  --c-red-600:   #b91c1c;

  --c-blue-400:  #60a5fa;
  --c-blue-500:  #3b82f6;
  --c-blue-600:  #1d4ed8;
}
```

#### 2.1.2 System tokens — Light

```css
:root {
  /* Surfaces */
  --bg-window:        var(--c-gray-100);
  --bg-card:          var(--c-gray-0);
  --bg-card-hover:    var(--c-gray-50);
  --bg-card-active:   var(--c-orange-50);   /* selected list row */
  --bg-overlay:       rgba(0, 0, 0, 0.04);
  --bg-pill:          rgba(0, 0, 0, 0.05);
  --bg-input:         var(--c-gray-0);
  --bg-sidebar:       var(--c-gray-100);
  --bg-topbar:        rgba(255, 255, 255, 0.72);  /* 毛玻璃用 */

  /* Text */
  --text-primary:     var(--c-gray-1000);
  --text-secondary:   var(--c-gray-600);
  --text-tertiary:    var(--c-gray-500);
  --text-on-accent:   #ffffff;
  --text-link:        var(--c-orange-600);

  /* Borders */
  --separator:        rgba(20, 20, 25, 0.10);
  --border-subtle:    rgba(0, 0, 0, 0.08);
  --border-strong:    rgba(0, 0, 0, 0.18);
  --border-focus:     var(--c-orange-500);

  /* Brand / Accent */
  --accent:           var(--c-orange-500);
  --accent-hover:     var(--c-orange-600);
  --accent-pressed:   var(--c-orange-700);
  --accent-soft:      var(--c-orange-100);
  --accent-soft-text: var(--c-orange-700);

  /* Semantics */
  --ok:               var(--c-green-500);
  --ok-soft:          rgba(34, 197, 94, 0.14);
  --warn:             var(--c-amber-500);
  --warn-soft:        rgba(245, 158, 11, 0.16);
  --danger:           var(--c-red-500);
  --danger-soft:      rgba(239, 68, 68, 0.12);
  --info:             var(--c-blue-500);
  --info-soft:        rgba(59, 130, 246, 0.12);

  /* Misc */
  --gray:             var(--c-gray-500);  /* "muted" status dot */
}
```

#### 2.1.3 System tokens — Dark

```css
@media (prefers-color-scheme: dark) {
  :root {
    --bg-window:        var(--c-gray-1000);
    --bg-card:          var(--c-gray-900);
    --bg-card-hover:    #232327;
    --bg-card-active:   rgba(255, 122, 26, 0.12);
    --bg-overlay:       rgba(255, 255, 255, 0.04);
    --bg-pill:          rgba(255, 255, 255, 0.08);
    --bg-input:         var(--c-gray-900);
    --bg-sidebar:       #131316;
    --bg-topbar:        rgba(20, 20, 22, 0.70);

    --text-primary:     #f5f5f7;
    --text-secondary:   #b0b0b6;
    --text-tertiary:    #8a8a92;
    --text-on-accent:   #ffffff;
    --text-link:        var(--c-orange-300);

    --separator:        rgba(255, 255, 255, 0.10);
    --border-subtle:    rgba(255, 255, 255, 0.08);
    --border-strong:    rgba(255, 255, 255, 0.18);
    --border-focus:     var(--c-orange-400);

    --accent:           var(--c-orange-500);
    --accent-hover:     var(--c-orange-400);
    --accent-pressed:   var(--c-orange-300);
    --accent-soft:      rgba(255, 122, 26, 0.16);
    --accent-soft-text: var(--c-orange-300);

    --ok-soft:          rgba(74, 222, 128, 0.18);
    --warn-soft:        rgba(251, 191, 36, 0.20);
    --danger-soft:      rgba(248, 113, 113, 0.18);
    --info-soft:        rgba(96, 165, 250, 0.18);
  }
}
```

#### 2.1.4 与 v0.1 现有 token 的迁移

| 现 v0.1 | v0.2 替换 | 备注 |
| --- | --- | --- |
| `--accent: #007aff` | `--accent: var(--c-orange-500)` | 主色由 iOS blue 改 LURA 橘 |
| `--orange: #ff9f0a` | 保留为 `--warn` 同义；新增 `--c-orange-*` 主色板 | 旧 CSS `var(--orange)` 仍能用，但建议改为 `var(--warn)` |
| `--bg-card-active: #f0f4ff` | `var(--c-orange-50)` | 选中行从冷色蓝改为暖橘 |
| 其他 | 不变 | text / separator / shadow 命名一字不改 |

### 2.2 Typography

字体栈延用现有：

```css
font-family:
  -apple-system, BlinkMacSystemFont,
  "SF Pro Text", "SF Pro",
  "Segoe UI Variable", "Segoe UI",
  system-ui, sans-serif;
```

等宽（用于 token / code / 端口号 / hash）：

```css
font-family: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
```

#### 2.2.1 Type scale

| Token | font-size / line-height / weight | 用途 |
| --- | --- | --- |
| `--font-display`  | 28 / 34 / 700 | 启动画面、空状态主标 |
| `--font-h1`       | 22 / 28 / 700 | 页面 Title（Dashboard / Accounts ...） |
| `--font-h2`       | 17 / 22 / 600 | 卡片头、Sheet 标题 |
| `--font-h3`       | 14 / 20 / 600 | 分组小标题 |
| `--font-body`     | 13 / 18 / 400 | 默认正文（与 v0.1 一致） |
| `--font-body-strong` | 13 / 18 / 500 | 行标题、按钮 |
| `--font-caption`  | 12 / 16 / 400 | 次要描述、subtitle |
| `--font-meta`     | 11.5 / 15 / 400 | usage 数字、辅助 meta |
| `--font-overline` | 11 / 14 / 600 / uppercase / +6% letter-spacing | section 标题 |
| `--font-mono`     | 11.5 / 16 / 400 / mono | 端口、token、code blob |

实现示例：

```css
:root {
  --font-h1: 700 22px/28px var(--font-sans);
  /* … */
}
.page-title { font: var(--font-h1); letter-spacing: -0.01em; }
```

### 2.3 间距（Spacing）

4 倍数 scale，所有 padding/margin/gap 只能取以下值：

```css
--space-0:  0;
--space-1:  2px;
--space-2:  4px;
--space-3:  6px;
--space-4:  8px;
--space-5: 10px;
--space-6: 12px;
--space-7: 14px;
--space-8: 16px;
--space-10:20px;
--space-12:24px;
--space-16:32px;
--space-20:40px;
--space-24:48px;
```

约定：
- 行内 gap 优先 `--space-4` / `--space-5`。
- 卡片内 padding 默认 `--space-7 --space-8`（14×16）。
- Section 之间垂直留白 `--space-12`（24）。

### 2.4 圆角（Radius）

```css
--radius-pill:    9999px;  /* pill / status dot ring */
--radius-button:  6px;
--radius-input:   6px;
--radius-row:     6px;
--radius-card:    10px;
--radius-modal:   14px;
--radius-mascot:  20px;   /* 大插画框 */
```

### 2.5 阴影 / Elevation

设计语言以"分隔线 + 卡片底色"取代厚重投影；只有 modal、kebab、tooltip、drawer 才上明显阴影。

```css
--shadow-card:    0 0 0 0.5px rgba(0,0,0,0.04), 0 1px 2px rgba(0,0,0,0.04);
--shadow-toolbar: inset 0 -0.5px 0 var(--separator);
--shadow-popover: 0 0 0 0.5px var(--border-subtle), 0 6px 20px rgba(0,0,0,0.18);
--shadow-modal:   0 0 0 0.5px var(--border-subtle), 0 24px 64px rgba(0,0,0,0.28);
--shadow-focus:   0 0 0 3px rgba(255, 122, 26, 0.30);
```

Dark mode 同名 token 用更暗的阴影色 + 更亮的 inset highlight。

### 2.6 图标

- **画布**：16×16，1.5px stroke，`stroke-linecap: round; stroke-linejoin: round;`（与 v0.1 一致）
- 大尺寸图标 24×24 时 stroke 升到 1.75px。
- **填充**：除了 status dot 与 brand mark，所有图标使用 `currentColor` + `fill="none"`，跟随上下文色。
- **命名**：`ICON_PASCAL_CASE`，导出 path d 字符串，详见 [src/App.tsx](../src/App.tsx) 的 `ICON_PLUS / ICON_COPY / ICON_CHECK / ICON_CHEVRON` 模式。
- **图标库**：v0.2 自维护一份 ~24 枚图标的 sprite（设计稿底部 Icons grid）。命名清单见 §8。

---

## 3. Components

每个组件给出：anatomy / states / props / token 引用。

### 3.1 Button

Anatomy：`[icon-leading?] [label] [icon-trailing?] / [spinner?]`

States：default / hover / active(pressed) / focus-visible / disabled / loading

Variants：

| Variant | 背景 | 文字 | 边框 | 用途 |
| --- | --- | --- | --- | --- |
| `btn-primary` | `--accent` → `--accent-hover` | `--text-on-accent` | inset 高光 | 唯一主操作 |
| `btn-secondary` | `--bg-card` | `--text-primary` | `inset 0 0 0 0.5px var(--border-subtle)` | 次要操作 |
| `btn-ghost` | transparent → `--bg-overlay` | `--text-secondary` → `--text-primary` | — | 工具按钮 |
| `btn-icon` | 同 ghost | secondary | 28×28 正方 | 顶栏、kebab |
| `btn-destructive` | `--bg-card` | `--danger` | subtle | 删除 |

尺寸：

| Size | height | padding | font |
| --- | --- | --- | --- |
| `sm` | 24 | 0 8 | body 12 |
| `md`（默认） | 28 | 0 12 | body 13 |
| `lg` | 36 | 0 16 | body-strong 14 |

Focus：`box-shadow: var(--shadow-focus)`，圆角与按钮一致。

### 3.2 Pill / Badge

```
height: 17 (sm) / 20 (md)
padding: 0 7 / 0 10
radius: var(--radius-pill)
font: 10.5px/14 600 (sm) | 11.5px/16 600 (md)
```

Tones：

| Class | bg | text |
| --- | --- | --- |
| `pill` | `--bg-pill` | `--text-secondary` |
| `pill.active-pill` | `--accent-soft` | `--accent-soft-text` |
| `pill.warn` | `--warn-soft` | `--warn` darker（light: `--c-amber-600`） |
| `pill.danger` | `--danger-soft` | `--c-red-600` |
| `pill.ok` | `--ok-soft` | `--c-green-600` |

### 3.3 Status Dot

```
size: 10 (md) | 7 (sm)
radius: 50%
ring: 0 0 0 2px <tone>-soft
```

| tone | bg |
| --- | --- |
| `ok` | `--ok` |
| `warn` | `--warn` |
| `danger` | `--danger` |
| `muted` | `--gray` |

### 3.4 Topbar

- 高度 52，padding `0 var(--space-10)`，背景 `--bg-topbar`，`backdrop-filter: blur(20px) saturate(180%)`。
- 下边沿 0.5px separator（`--shadow-toolbar`）。
- 内容三区：left（page title + breadcrumb / cred status pill）、center（空 / 搜索）、right（Auto Switch toggle / icon buttons / avatar）。
- `position: sticky; top: 0; z-index: 10`。

### 3.5 Sidebar

- 宽度 220（展开） / 64（折叠），高度 `100vh`。
- 背景 `--bg-sidebar`，右边沿 0.5px separator。
- 顶部 logo 区高 52（与 topbar 对齐）。
- 导航项：高 36，padding `0 var(--space-8)`，icon 18，gap 10。
  - Default：text-secondary。
  - Hover：bg `--bg-overlay`，text-primary。
  - Active（当前路由）：bg `--accent-soft`，text `--accent-soft-text`，左侧 3px 圆角竖条 `--accent`。
- 底部 daemon health 区域：dot + "Daemon" + 版本号；hover 弹 popover 显示端口/PID。

### 3.6 Card

```
background: var(--bg-card)
radius: var(--radius-card)
shadow: var(--shadow-card)
padding: var(--space-7) var(--space-8)
```

Card header anatomy：`[icon] [title] [right-action]`，title 使用 `--font-h2`，下面留 `--space-4`。

### 3.7 KPI / Stat Card

```
[icon-soft]   [eyebrow caption]
              [big number]                [trend pill ↑↓]
              [secondary line]
```

- 背景 `--bg-card`，padding `--space-8`。
- icon-soft：32×32 圆角方，bg = 对应 tone 的 soft 色，icon = tone 主色。
- big number：`--font-h1`，`font-variant-numeric: tabular-nums`。
- trend pill：`pill.ok` 或 `pill.danger`，含 ↑ / ↓ 字符。

### 3.8 Input

```
height: 28 (md) | 32 (lg)
padding: 0 10
radius: var(--radius-input)
background: var(--bg-input)
border: inset 0 0 0 0.5px var(--border-subtle)
focus: + var(--shadow-focus)
```

- Mono 输入（OAuth code、port）加 `.mono` class，使用 mono 字体 11.5px。
- Search input 带前置 search icon、可选清除按钮。
- 错误态：`box-shadow: inset 0 0 0 1px var(--danger), 0 0 0 3px var(--danger-soft);`

### 3.9 Toggle / Switch

```
width: 36
height: 22
radius: pill
track-off: var(--bg-pill)
track-on:  var(--accent)
knob: 18×18 white, shadow 0 1 1 rgba(0,0,0,.18)
transition: 0.18s ease
```

Disabled：track 透明度 0.4。Focus：3px focus ring。

### 3.10 List Row（沿用 v0.1）

继续使用 `.list .row` + 5 列 grid（`auto 1fr auto auto auto`）。改动：

- `.row.active` 背景从 `#f0f4ff` 改为 `--bg-card-active`（橘色 50）。
- 行高从 11 14 改为 12 14（更舒展）。
- 行间分隔线保留 0.5px `--separator`。

### 3.11 Usage Bar（沿用 v0.1）

```
height: 5
track-bg: rgba(0,0,0,0.06) / dark: rgba(255,255,255,0.08)
fill: ok | warn | danger
transition: width 0.4s ease
```

Aria：包裹元素加 `role="progressbar"` + `aria-valuenow/min/max`。

### 3.12 Sheet / Modal

- Backdrop：`rgba(0,0,0,0.36)` + `backdrop-filter: blur(8px)`。
- Container：`--bg-card`，`--radius-modal`，`--shadow-modal`，max-width 480 / 600 / 800（sm/md/lg）。
- Header：`--font-h2` + close 按钮（icon X）。
- Body 与 footer 之间 0.5px separator。
- Footer 右对齐，secondary 在左、primary 在右，gap `--space-4`。
- Esc / 点击 backdrop / Cancel 都可关闭。

### 3.13 Drawer（v0.2 新增）

右侧固定面板，宽 420（≥1280px 常驻）/ overlay（1024–1279px）/ full screen（<1024px）。

- 进出动画：transform translateX 280ms cubic-bezier(0.32, 0.72, 0, 1)。
- Header 固定，body 内滚动。

### 3.14 Kebab Menu（沿用 v0.1）

`.kebab-menu` + `.kebab-item`，`--shadow-popover`，min-width 160。danger 项使用 `--danger` 字色 + hover bg `--danger-soft`。

### 3.15 Skeleton（沿用 v0.1）

`sk-bar` shimmer + `sk-dot` pulse；动画时长 1.4s linear infinite。

### 3.16 Banner / Toast

Banner：内嵌在页面顶部，水平占满。

| Variant | bg | text |
| --- | --- | --- |
| `banner.info` | `--info-soft` | `--info` |
| `banner.warn` | `--warn-soft` | `--c-amber-600` |
| `banner.err` | `--danger-soft` | `--danger` |

Toast：右下角浮层，宽 ≤ 360，自动 4s 消失，可手动关闭，`--shadow-popover`。

### 3.17 Empty State

```
[Mascot 96×96]
[H2 标题]
[caption 描述]
[primary CTA]
```

- 居中，垂直 padding `--space-20`。
- Mascot 使用 LURA full body SVG。

### 3.18 Chart

- 折线图：使用 `--accent` / `--ok` / `--info` 三色区分 5h / 7d Opus / 7d Sonnet。
- 网格线：`--separator`，1px。
- 轴文字：`--font-meta`，`--text-tertiary`。
- Tooltip：`--bg-card` + `--shadow-popover`，包含点状态点 + 数值 + 时间。
- Sparkline（KPI 卡内）：高度 28，无轴，仅一条 `--accent` 折线 + 末端圆点。

### 3.19 Avatar

```
size: 28 (sm) | 36 (md) | 48 (lg)
radius: 50%
```

- 优先头像图（OAuth profile picture，若日后加）。
- 退化 → 首字母 + 来自 name hash 的底色（取 `--c-orange-300` / `--c-blue-400` / `--c-green-400` / `--c-amber-400` / `--c-red-400` 五色之一）。
- 当前注入账号头像加 2px `--accent` 描边。

### 3.20 Tab / Filter Chip（Activity 页）

```
height: 26
padding: 0 12
radius: pill
default: bg --bg-pill, text --text-secondary
active:  bg --accent-soft, text --accent-soft-text
```

---

## 4. Layout

### 4.1 网格

- 主内容区最大宽度 1120，超出居中留白；最小宽度 640。
- 列网格：12 列，gutter 16，column-min 56。
- 卡片之间垂直 gap `--space-10`，section 之间 gap `--space-12`。

### 4.2 页面骨架

```
┌────────────────────────────────────────────────────────────┐
│ Topbar                                                     │
├──────────┬─────────────────────────────────┬──────────────┤
│          │                                 │              │
│ Sidebar  │ Main scroll area                │ Drawer       │
│ 220 / 64 │ flex 1, padding 24              │ 420 (opt.)   │
│          │                                 │              │
└──────────┴─────────────────────────────────┴──────────────┘
```

### 4.3 响应式断点（与 PRD §8 一致）

```css
--bp-sm: 768px;
--bp-md: 1024px;
--bp-lg: 1280px;
```

---

## 5. Motion

| Token | duration | easing | 用途 |
| --- | --- | --- | --- |
| `--motion-quick` | 120ms | `ease` | 颜色 / 阴影 hover |
| `--motion-base`  | 180ms | `cubic-bezier(0.32, 0.72, 0, 1)` | toggle / kebab open |
| `--motion-page`  | 280ms | `cubic-bezier(0.32, 0.72, 0, 1)` | drawer / modal |
| `--motion-shimmer` | 1400ms linear infinite | — | skeleton |
| `--motion-spring` | 400ms `cubic-bezier(0.34, 1.56, 0.64, 1)` | — | KPI 数字 count-up |

约定：

- 只有用户主动操作触发的元素才动画，列表数据进入 / 重排不做动画。
- `prefers-reduced-motion: reduce` 时全部缩到 1ms（保留状态变化但去掉过渡）。

---

## 6. 无障碍

- 所有交互元素 `tabindex` 自然顺序，焦点可见（`outline: none` 必须配 `box-shadow: var(--shadow-focus)`）。
- 颜色对比度：正文 ≥ 4.5:1，pill / caption ≥ 3:1，状态点对底色 ≥ 3:1。
- 状态信息**不能仅靠颜色**——状态点必须配 aria-label 或紧邻的文字 pill。
- 表单：每个 input 有 visible label 或 aria-label；错误信息 `aria-describedby` 关联。
- 图标按钮必须 `aria-label`。
- `prefers-reduced-motion` / `prefers-color-scheme` 都尊重。
- Modal 打开时焦点陷阱 + Esc 关闭 + restore focus to trigger。

---

## 7. Token 落地清单

工程交付步骤（按顺序）：

1. **`src/styles/tokens.css`**（新建）：把 §2.1.1 / 2.1.2 / 2.1.3 / 2.3 / 2.4 / 2.5 / Motion 的所有 CSS variables 集中到一个文件，由 `src/main.tsx` 第一行 import。
2. **`src/styles.css`**（改）：删掉 v0.1 内联的 `:root` 颜色块，全部改用 system token。保留 `.app .toolbar .row .pill` 等组件级 class，但底层颜色全替换为新 token（特别是 `--accent` 切到橘）。
3. **`src/styles/typography.css`**（新建）：把 `--font-*` 定义和 `.text-*` utility class 放进来。
4. **`src/components/`**（新建）：依据 §3 拆出 `Button`, `Pill`, `StatusDot`, `Card`, `StatCard`, `Toggle`, `Sidebar`, `Topbar`, `Drawer`, `Banner`, `EmptyState` 等基础组件。每个组件一个文件，class 命名跟本文一致。
5. **`src/icons/`**（新建）：把 §1.3 / §3 / §8 列出的全部图标 d 字符串 + `<Icon>` 组件放进来。
6. **`src-tauri/icons/`**：替换为 LURA 设计稿导出的多分辨率图标。
7. 主题切换：`<html data-theme="light|dark|system">` + JS 在 system 时跟随 `prefers-color-scheme`。tokens.css 用 `[data-theme="dark"]` 与 `@media (prefers-color-scheme: dark)` 双路径覆盖。

---

## 8. 图标库（v0.2 必交付清单）

按设计稿底部 Icons grid 罗列：

| 名称 | 用途 |
| --- | --- |
| `plus` | 新增 |
| `copy` | 复制 OAuth URL |
| `check` | 完成 |
| `chevron-right` / `chevron-down` | 展开收起 |
| `dots-horizontal` | Kebab |
| `search` | Topbar 搜索 |
| `bell` | 通知（占位） |
| `gear` | Settings |
| `dashboard` | Sidebar Dashboard |
| `users` | Sidebar Accounts |
| `pulse` | Sidebar Activity |
| `home` | （备用） |
| `refresh` | 主动刷新 |
| `power` | 启停 daemon |
| `trash` | 删除 |
| `external-link` | 打开外部 URL |
| `info` | banner.info |
| `warning` | banner.warn |
| `error` | banner.err |
| `eye` / `eye-off` | 显示/隐藏敏感信息 |
| `clock` | last used / cooldown |
| `sun` / `moon` | 主题切换 |
| `keyboard` | 快捷键 |
| `arrow-up-right` / `arrow-down-right` | trend pill |

每枚都按 §2.6 的画布与 stroke 规则绘制；最终 `src/icons/index.ts` 导出 path 字符串常量 + 一个 `<Icon name="…">` 组件。

---

## 9. 交付清单（设计 → 工程 hand-off）

设计师交付：

- [ ] `docs/design.png` 高清版（≥ 4096×2730，方便读小字）。
- [ ] LURA Mark / Mascot / Sticker SVG 源文件。
- [ ] App icon 多平台导出（icns / ico / png 1024 / Android adaptive 双层）。
- [ ] 全部 §8 图标的 SVG（path d 已优化，去掉 transform）。
- [ ] Empty state 三张插画（No accounts / All cooled down / Daemon offline）。
- [ ] Figma 文件链接 + 颜色 / 文字样式同名于本文 token。

工程产出：

- [ ] `docs/PRD.md`（本仓库已落地）
- [ ] `docs/DESIGN_SYSTEM.md`（本文）
- [ ] `src/styles/tokens.css`、`src/styles/typography.css`
- [ ] `src/components/*` 基础组件
- [ ] `src/icons/*` 图标常量
- [ ] Storybook 或 `src/__playground__/index.tsx`：渲染所有组件 light/dark 双主题，便于 visual diff。

---

## 10. 待对齐项（与 PRD §13 同步）

| # | 问题 | 当前默认 |
| --- | --- | --- |
| 1 | 设计图分辨率有限，部分色值是从语义反推；上线前需 Figma 取色二次确认 | 见 §2.1.1 推荐值 |
| 2 | "Persona Score" 子页含义不明 | 实现为 Account 详情 / 单账号分析 |
| 3 | Mascot SVG 是否已交付源文件 | 否，工程暂用占位 |
| 4 | 是否要支持自定义主题色 | 否，仅 LURA 橘 |
| 5 | Charts 是否上图表库（recharts / visx） | 推荐 visx，体积 < 60kb |
