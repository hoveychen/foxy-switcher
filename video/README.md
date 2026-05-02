# video/ — Foxy Switcher intro video

Anthropic-aesthetic Remotion intro video for Foxy Switcher. Two outputs from one composition:

- **mp4** — shareable file (`out/foxy-intro.mp4`) for Twitter / Slack / README embed.
- **`<Player>` web component** — static bundle in `dist/` for GitHub Pages, plus a reusable React component (`FoxyIntroPlayer`) for the Tauri app's onboarding screen.

## Composition

- 45 s @ 30 fps (1350 frames), 1920×1080.
- Single composition id: `FoxyIntro` (see [src/Root.tsx](src/Root.tsx)).
- Five beats: Pain Scenario (10 s, layered cascade) → Idea Pivot (6 s) → Add Accounts (9 s) → Auto Rotation (12 s) → CTA Outro (8 s).
- BGM: HoliznaCC0 — *Happy Dance* (CC0, 118 BPM, no attribution required).

## Build

```sh
# install once
npm install

# render mp4
npm run render                 # → out/foxy-intro.mp4
npm run render:preview         # half-resolution preview

# preview in Remotion Studio
npm run studio                 # http://localhost:3000

# Player bundle for GitHub Pages
npm run dev                    # vite dev server, http://localhost:5173
npm run build:player           # static bundle in dist/
```

`npm run build:player` produces a self-contained static site:

```
dist/
├── index.html
├── assets/index-<hash>.js     # ~115 kB gzip (React + @remotion/player + composition)
├── bgm.mp3                    # ~1.1 MB
└── foxy-icon.png
```

Drop `dist/` on any static host (GitHub Pages, Netlify, Vercel) — no server runtime needed.

## Embed `FoxyIntroPlayer` in another React app

```tsx
import { FoxyIntroPlayer } from 'foxy-switcher-video/embed';

<FoxyIntroPlayer
  width="100%"      // CSS width or pixels; aspect ratio locks to 16/9
  autoPlay
  loop
  controls
  clickToPlay
/>
```

For the Tauri onboarding flow, import `FoxyIntroPlayer` directly from this subdirectory (or publish it to a private npm registry). The composition runs purely in React — no native dependencies, no separate window.

## Self-check protocol

After every `npm run render`, extract key frames with ffmpeg and read each one before declaring the cut ready:

```sh
mkdir -p /tmp/check
for t in 1 5 8 11 13 15 22 30 37 44; do
  ffmpeg -ss $t -i out/foxy-intro.mp4 -frames:v 1 -y /tmp/check/t${t}s.png 2>/dev/null
done
open /tmp/check    # eyeball each frame
```

The composition shares helpers between Player and `renderMedia` paths, so the mp4 and the Player should always agree visually.

## Layout

```
video/
├── package.json              # both Remotion + Vite scripts live here
├── remotion.config.ts        # codec + crf for `remotion render`
├── vite.config.ts            # Player bundle config
├── tsconfig.json
├── index.html                # Vite entry for the Player site
├── public/
│   ├── bgm.mp3               # 47 s trim of Happy Dance (0.5 s in / 2 s out fade)
│   └── foxy-icon.png         # LURA mascot
├── assets/                   # source mp3s, beat tracks (not shipped)
├── src/
│   ├── index.ts              # Remotion CLI registerRoot entry
│   ├── Root.tsx              # registers the FoxyIntro composition
│   ├── FoxyIntro.tsx         # the composition (5 scenes + helpers)
│   └── embed/
│       ├── PlayerApp.tsx     # `<FoxyIntroPlayer>` reusable component
│       └── main.tsx          # Vite mount entry
├── out/                      # rendered mp4 lands here (gitignored)
└── dist/                     # vite build output (gitignored)
```

## Brand tokens

Defined in [src/FoxyIntro.tsx](src/FoxyIntro.tsx) `COLORS`. The accent uses Foxy's own LURA orange (`#FF7A1A`) rather than Anthropic coral, so the piece reads as Foxy-branded, not as Anthropic official content.

| Token        | Value     | Use                                                |
| ------------ | --------- | -------------------------------------------------- |
| `cream`      | `#E5DCCE` | Cream backdrop for title / pivot / outro beats     |
| `bone`       | `#FAF7F1` | Demo / GUI mock backdrop with subtle grid          |
| `orange`     | `#FF7A1A` | Trailing-word emphasis, ripples, primary buttons   |
| `orangeDeep` | `#B94A00` | "Wait —" idea spark text                           |
| `ink`        | `#1A1A1A` | Body text                                          |
| `red`        | `#D24B3A` | Rate-limit / Sonnet-cap modals                     |
| `green`      | `#2F9E5A` | Healthy / passing test ✓ states                    |

Type stack: Fraunces (display + italic), Inter (UI labels), JetBrains Mono (terminal + code).
