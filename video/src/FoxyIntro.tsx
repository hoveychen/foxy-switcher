import {
  AbsoluteFill,
  Audio,
  Img,
  Series,
  interpolate,
  staticFile,
  useCurrentFrame,
} from 'remotion';
import { loadFont as loadFraunces } from '@remotion/google-fonts/Fraunces';
import { loadFont as loadInter } from '@remotion/google-fonts/Inter';
import { loadFont as loadJetBrainsMono } from '@remotion/google-fonts/JetBrainsMono';

const { fontFamily: serif } = loadFraunces('normal', {
  weights: ['400'],
  subsets: ['latin'],
});
const { fontFamily: serifItalic } = loadFraunces('italic', {
  weights: ['400'],
  subsets: ['latin'],
});
void serifItalic;
const { fontFamily: sans } = loadInter('normal', {
  weights: ['400', '500', '600', '700'],
  subsets: ['latin'],
});
const { fontFamily: mono } = loadJetBrainsMono('normal', {
  weights: ['400', '500', '600'],
  subsets: ['latin'],
});

export const COLORS = {
  cream: '#E5DCCE',
  bone: '#FAF7F1',
  ink: '#1A1A1A',
  inkSoft: '#5C5A55',
  inkGhost: '#B6B0A4',
  orange: '#FF7A1A',
  orangeDeep: '#B94A00',
  orangeSoft: '#FFE8D1',
  cardBorder: '#E5DFD2',
  cardBorderSoft: '#EFE9DD',
  cardWhite: '#FFFFFF',
  termBg: '#1F1B17',
  termText: '#F5EFE3',
  green: '#2F9E5A',
  red: '#D24B3A',
  shadow: 'rgba(26,26,26,0.08)',
};

const W = 1920;
const H = 1080;

const ease = (t: number) => 1 - Math.pow(1 - t, 3);
const easeInOut = (t: number) =>
  t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
const clamp01 = (v: number) => Math.min(1, Math.max(0, v));

const useEnter = (start: number, durationFrames = 18) => {
  const frame = useCurrentFrame();
  const local = Math.max(0, frame - start);
  const t = clamp01(local / durationFrames);
  const eased = ease(t);
  return { opacity: eased, lift: (1 - eased) * 12, t: eased };
};

const useExit = (end: number, durationFrames = 12) => {
  const frame = useCurrentFrame();
  const local = Math.max(0, end - frame);
  const t = clamp01(local / durationFrames);
  const eased = ease(1 - t);
  return { opacity: 1 - eased };
};

// ─────────────────────────────────────────────────────────────
// SHARED — Backgrounds
// ─────────────────────────────────────────────────────────────
const CreamBackground: React.FC<{ opacity?: number }> = ({ opacity = 1 }) => (
  <AbsoluteFill style={{ backgroundColor: COLORS.cream, opacity }} />
);

const GridBackground: React.FC<{ opacity?: number }> = ({ opacity = 1 }) => {
  const cell = 88;
  const line = 'rgba(26, 26, 26, 0.05)';
  return (
    <AbsoluteFill
      style={{
        backgroundColor: COLORS.bone,
        backgroundImage: `linear-gradient(${line} 1px, transparent 1px), linear-gradient(90deg, ${line} 1px, transparent 1px)`,
        backgroundSize: `${cell}px ${cell}px`,
        opacity,
      }}
    />
  );
};

// ─────────────────────────────────────────────────────────────
// SHARED — Mouse cursor
// ─────────────────────────────────────────────────────────────
const MouseCursor: React.FC<{
  x: number;
  y: number;
  opacity?: number;
  scale?: number;
}> = ({ x, y, opacity = 1, scale = 1 }) => (
  <svg
    width={28 * scale}
    height={34 * scale}
    viewBox="0 0 28 34"
    style={{
      position: 'absolute',
      left: x,
      top: y,
      opacity,
      pointerEvents: 'none',
      filter: 'drop-shadow(0 2px 4px rgba(0,0,0,0.18))',
    }}
  >
    <path
      d="M3 2 L3 26 L9 21 L13 31 L17 30 L13 20 L22 20 Z"
      fill="#1A1A1A"
      stroke="#FFFFFF"
      strokeWidth="1.6"
      strokeLinejoin="round"
    />
  </svg>
);

const ClickRipple: React.FC<{
  x: number;
  y: number;
  start: number;
  color?: string;
}> = ({ x, y, start, color = COLORS.orange }) => {
  const f = useCurrentFrame();
  const t = clamp01((f - start) / 16);
  if (t <= 0 || t >= 1) return null;
  const eased = ease(t);
  const size = interpolate(eased, [0, 1], [16, 110]);
  const opacity = (1 - eased) * 0.6;
  return (
    <div
      style={{
        position: 'absolute',
        left: x - size / 2,
        top: y - size / 2,
        width: size,
        height: size,
        borderRadius: '50%',
        border: `2.5px solid ${color}`,
        opacity,
        pointerEvents: 'none',
      }}
    />
  );
};

// ─────────────────────────────────────────────────────────────
// SHARED — Typed text + caret
// ─────────────────────────────────────────────────────────────
const BlinkCaret: React.FC<{ height?: number; color?: string }> = ({
  height = 32,
  color = COLORS.orange,
}) => {
  const frame = useCurrentFrame();
  const visible = Math.floor(frame / 14) % 2 === 0;
  return (
    <span
      style={{
        display: 'inline-block',
        width: 12,
        height,
        marginLeft: 4,
        marginBottom: -3,
        background: color,
        verticalAlign: 'middle',
        opacity: visible ? 1 : 0.15,
      }}
    />
  );
};

const TypedText: React.FC<{
  text: string;
  start: number;
  speedFrames?: number;
  style?: React.CSSProperties;
}> = ({ text, start, speedFrames = 3, style }) => {
  const frame = useCurrentFrame();
  const elapsed = Math.max(0, frame - start);
  const charsShown = Math.min(text.length, Math.floor(elapsed / speedFrames));
  return <span style={style}>{text.slice(0, charsShown)}</span>;
};

// ─────────────────────────────────────────────────────────────
// SHARED — Spinner / Check
// ─────────────────────────────────────────────────────────────
const Spinner: React.FC<{ size?: number; color?: string }> = ({
  size = 28,
  color = COLORS.orange,
}) => {
  const frame = useCurrentFrame();
  return (
    <svg width={size} height={size} viewBox="0 0 24 24">
      <circle cx="12" cy="12" r="9" stroke={COLORS.cardBorder} strokeWidth="2.6" fill="none" />
      <g transform={`rotate(${frame * 9} 12 12)`}>
        <path
          d="M21 12 A9 9 0 0 0 12 3"
          stroke={color}
          strokeWidth="2.6"
          fill="none"
          strokeLinecap="round"
        />
      </g>
    </svg>
  );
};

const CheckCircle: React.FC<{ size?: number; appear: number; color?: string }> = ({
  size = 28,
  appear,
  color = COLORS.green,
}) => {
  const eased = ease(appear);
  const dash = 12;
  return (
    <svg width={size} height={size} viewBox="0 0 24 24">
      <circle
        cx="12"
        cy="12"
        r="10"
        fill={color}
        opacity={eased}
        transform={`scale(${0.8 + 0.2 * eased})`}
        style={{ transformOrigin: '12px 12px', transformBox: 'fill-box' }}
      />
      <path
        d="M7.5 12.5 L11 16 L17 9.5"
        stroke="#fff"
        strokeWidth="2.4"
        fill="none"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeDasharray={dash}
        strokeDashoffset={dash * (1 - eased)}
      />
    </svg>
  );
};

// ─────────────────────────────────────────────────────────────
// SHARED — Terminal window mock
// ─────────────────────────────────────────────────────────────
const TerminalWindow: React.FC<{
  children: React.ReactNode;
  width?: number;
  height?: number;
  title?: string;
  shadow?: boolean;
}> = ({ children, width = 1100, height = 600, title = '~ — claude', shadow = true }) => (
  <div
    style={{
      width,
      height,
      background: COLORS.termBg,
      border: `1px solid #2C2620`,
      borderRadius: 14,
      boxShadow: shadow ? `0 30px 70px rgba(0,0,0,0.22)` : 'none',
      overflow: 'hidden',
    }}
  >
    <div
      style={{
        height: 36,
        background: '#26211C',
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        padding: '0 16px',
      }}
    >
      <span style={{ width: 12, height: 12, borderRadius: 6, background: '#E0654E' }} />
      <span style={{ width: 12, height: 12, borderRadius: 6, background: '#E5C24B' }} />
      <span style={{ width: 12, height: 12, borderRadius: 6, background: '#7FB36B' }} />
      <span style={{ marginLeft: 14, fontFamily: mono, fontSize: 13, color: '#897F70' }}>
        {title}
      </span>
    </div>
    <div
      style={{
        padding: '28px 32px',
        fontFamily: mono,
        fontSize: 22,
        color: COLORS.termText,
        lineHeight: 1.6,
      }}
    >
      {children}
    </div>
  </div>
);

// Browser window mock (Chrome-style address bar)
const BrowserWindow: React.FC<{
  children: React.ReactNode;
  url: string;
  width?: number;
  height?: number;
}> = ({ children, url, width = 1000, height = 560 }) => (
  <div
    style={{
      width,
      height,
      background: '#FFFFFF',
      border: `1px solid ${COLORS.cardBorderSoft}`,
      borderRadius: 12,
      boxShadow: `0 30px 70px rgba(0,0,0,0.22)`,
      overflow: 'hidden',
    }}
  >
    <div
      style={{
        height: 44,
        background: '#F2EFEA',
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        padding: '0 16px',
        borderBottom: `1px solid ${COLORS.cardBorderSoft}`,
      }}
    >
      <span style={{ width: 11, height: 11, borderRadius: 5.5, background: '#E0654E' }} />
      <span style={{ width: 11, height: 11, borderRadius: 5.5, background: '#E5C24B' }} />
      <span style={{ width: 11, height: 11, borderRadius: 5.5, background: '#7FB36B' }} />
      <div
        style={{
          marginLeft: 18,
          flex: 1,
          height: 26,
          borderRadius: 13,
          background: '#FFFFFF',
          border: `1px solid ${COLORS.cardBorder}`,
          fontFamily: mono,
          fontSize: 12,
          color: COLORS.inkSoft,
          display: 'flex',
          alignItems: 'center',
          padding: '0 12px',
        }}
      >
        🔒 {url}
      </div>
    </div>
    <div style={{ padding: '32px 36px' }}>{children}</div>
  </div>
);

// ─────────────────────────────────────────────────────────────
// SCENE 1 — PAIN SCENARIO (0 → 300 frames, 10s, layered cascade)
// All sub-windows accumulate on screen instead of replacing each other.
// Each new window enters before the previous finishes, so by the end we
// see a cluttered cascade of 6+ rate-limit / OAuth / logout windows.
// ─────────────────────────────────────────────────────────────

const PainWindow: React.FC<{
  start: number;
  x: number;
  y: number;
  rot?: number;
  scale?: number;
  z?: number;
  children: React.ReactNode;
}> = ({ start, x, y, rot = 0, scale = 1, z = 0, children }) => {
  const f = useCurrentFrame();
  const t = ease(clamp01((f - start) / 12));
  if (t <= 0) return null;
  return (
    <div
      style={{
        position: 'absolute',
        left: x,
        top: y,
        transform: `scale(${scale * (0.92 + 0.08 * t)}) rotate(${rot}deg)`,
        transformOrigin: 'top left',
        opacity: t,
        zIndex: z,
      }}
    >
      {children}
    </div>
  );
};

const PainScenarioScene: React.FC = () => {
  const f = useCurrentFrame();
  const enter = useEnter(0, 8);
  const exit = useExit(300, 14);
  const baseOpacity = Math.min(enter.opacity, exit.opacity);

  // Cursor zips around clicking each new window in turn.
  const cursorPath = [
    { f: 0, x: 1700, y: 950 },
    { f: 24, x: 760, y: 460 }, // arrive at main terminal
    { f: 50, x: 1380, y: 220 }, // up to rate limit pop
    { f: 80, x: 540, y: 640 }, // logout terminal
    { f: 120, x: 1340, y: 380 }, // browser
    { f: 160, x: 880, y: 760 }, // paste terminal
    { f: 200, x: 1380, y: 600 }, // second cap
    { f: 240, x: 600, y: 220 }, // chaos
    { f: 270, x: 1500, y: 880 }, // chaos
    { f: 300, x: 1850, y: 1100 }, // exit
  ];
  let segIdx = 0;
  for (let i = 0; i < cursorPath.length - 1; i++) {
    if (f >= cursorPath[i].f && f < cursorPath[i + 1].f) {
      segIdx = i;
      break;
    }
  }
  const seg = cursorPath[segIdx];
  const next = cursorPath[Math.min(segIdx + 1, cursorPath.length - 1)];
  const segT = easeInOut(clamp01((f - seg.f) / Math.max(1, next.f - seg.f)));
  const cx = interpolate(segT, [0, 1], [seg.x, next.x]);
  const cy = interpolate(segT, [0, 1], [seg.y, next.y]);
  const cursorOpacity = f < 280 ? clamp01((f - 4) / 10) : clamp01((300 - f) / 14);

  const clicks = [22, 52, 82, 122, 162, 202, 240, 268];

  // Cascade of small error popups in the last 90 frames
  const cascadeItems = [
    { x: 80, y: 50, rot: -3, delay: 200, label: 'Rate limit' },
    { x: 1500, y: 90, rot: 2, delay: 212, label: 'Logged out.' },
    { x: 60, y: 460, rot: 1, delay: 224, label: 'Authorize?' },
    { x: 1480, y: 520, rot: -2, delay: 236, label: 'Paste code' },
    { x: 360, y: 950, rot: 1.5, delay: 248, label: '✗ Sonnet cap' },
    { x: 1100, y: 940, rot: -2.5, delay: 260, label: 'Try again' },
    { x: 940, y: 60, rot: 2.5, delay: 272, label: 'OAuth code…' },
  ];

  return (
    <AbsoluteFill>
      <GridBackground />
      <AbsoluteFill style={{ opacity: baseOpacity }}>
        {/* (1) Main coding terminal — entry 0 */}
        <PainWindow start={0} x={120} y={140} rot={-1.5} scale={0.78} z={1}>
          <TerminalWindow width={900} height={520} title="~/projects/acme — claude">
            <div style={{ color: '#897F70', marginBottom: 8 }}>
              <span style={{ color: COLORS.orange }}>$</span>{' '}
              <TypedText text='claude "fix the failing auth test"' start={6} speedFrames={1} />
            </div>
            <div
              style={{
                background: '#2C2620',
                border: `1px solid #3A3328`,
                borderRadius: 10,
                padding: '14px 18px',
                marginTop: 12,
                opacity: clamp01((f - 16) / 8),
              }}
            >
              <div style={{ fontSize: 16, color: '#A8A096', marginBottom: 8, letterSpacing: 1 }}>
                PROGRESS
              </div>
              {[
                { label: 'Reading session.ts', start: 18, dur: 8 },
                { label: 'Patching token check', start: 26, dur: 8 },
                { label: '247 tests pass', start: 34, dur: 8 },
              ].map((s, i) => {
                const localStart = Math.max(0, f - s.start);
                const checkAppear = clamp01((localStart - s.dur) / 6);
                const isPending = f < s.start - 2;
                const isRunning = f >= s.start - 2 && f < s.start + s.dur;
                const isDone = f >= s.start + s.dur;
                return (
                  <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 4 }}>
                    <div style={{ width: 22, height: 22 }}>
                      {isPending && <span style={{ color: '#5C5A55' }}>·</span>}
                      {isRunning && <Spinner size={20} color={COLORS.orange} />}
                      {isDone && <CheckCircle size={20} appear={checkAppear} color={COLORS.green} />}
                    </div>
                    <div
                      style={{
                        fontSize: 16,
                        color: isDone ? '#A8A096' : isRunning ? COLORS.termText : '#5C5A55',
                      }}
                    >
                      {s.label}
                    </div>
                  </div>
                );
              })}
            </div>
          </TerminalWindow>
        </PainWindow>

        {/* (2) Rate limit modal — entry 30 */}
        <PainWindow start={30} x={1100} y={120} rot={2.5} scale={0.85} z={3}>
          <div
            style={{
              width: 620,
              background: '#FFFFFF',
              border: `2.5px solid ${COLORS.red}`,
              borderRadius: 14,
              padding: '22px 26px',
              boxShadow: '0 26px 60px rgba(210,75,58,0.32)',
            }}
          >
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 12,
                fontFamily: sans,
                fontWeight: 700,
                fontSize: 24,
                color: COLORS.red,
                marginBottom: 8,
              }}
            >
              <span
                style={{
                  width: 30,
                  height: 30,
                  borderRadius: 15,
                  background: COLORS.red,
                  color: '#fff',
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  fontSize: 20,
                }}
              >
                !
              </span>
              Rate limit reached
            </div>
            <div style={{ fontFamily: sans, fontSize: 17, color: COLORS.inkSoft }}>
              5-hour cap. Try again in <strong style={{ color: COLORS.ink }}>4h 32m</strong>.
            </div>
          </div>
        </PainWindow>

        {/* (3) Logout/login terminal — entry 60 */}
        <PainWindow start={60} x={350} y={550} rot={-2} scale={0.7} z={2}>
          <TerminalWindow width={780} height={360} title="~ — claude">
            <div style={{ fontSize: 19 }}>
              <span style={{ color: COLORS.orange }}>$</span>{' '}
              <TypedText text="claude logout" start={64} speedFrames={1} />
            </div>
            <div
              style={{ fontSize: 19, color: '#897F70', marginTop: 6, opacity: clamp01((f - 78) / 4) }}
            >
              Logged out.
            </div>
            <div style={{ fontSize: 19, marginTop: 6, opacity: clamp01((f - 86) / 4) }}>
              <span style={{ color: COLORS.orange }}>$</span>{' '}
              <TypedText text="claude login" start={90} speedFrames={1} />
            </div>
            <div
              style={{
                fontSize: 17,
                color: COLORS.orange,
                marginTop: 10,
                opacity: clamp01((f - 108) / 4),
              }}
            >
              https://claude.ai/oauth/authorize?…
            </div>
          </TerminalWindow>
        </PainWindow>

        {/* (4) Browser OAuth — entry 90 */}
        <PainWindow start={90} x={1080} y={300} rot={1.5} scale={0.7} z={4}>
          <BrowserWindow url="claude.ai/oauth/authorize" width={780} height={420}>
            <div style={{ fontFamily: sans, fontSize: 22, color: COLORS.ink, marginBottom: 14, fontWeight: 600 }}>
              Authorize Claude Code
            </div>
            <div style={{ fontFamily: sans, fontSize: 16, color: COLORS.inkSoft, lineHeight: 1.5 }}>
              Allow Claude Code to access your account?
            </div>
            <div style={{ display: 'flex', gap: 12, marginTop: 24 }}>
              <div
                style={{
                  background: '#FFFFFF',
                  border: `1px solid ${COLORS.cardBorder}`,
                  color: COLORS.inkSoft,
                  padding: '10px 18px',
                  borderRadius: 10,
                  fontFamily: sans,
                  fontSize: 14,
                }}
              >
                Cancel
              </div>
              <div
                style={{
                  background: COLORS.orange,
                  color: '#fff',
                  padding: '10px 18px',
                  borderRadius: 10,
                  fontFamily: sans,
                  fontWeight: 600,
                  fontSize: 14,
                }}
              >
                Authorize
              </div>
            </div>
            <div
              style={{
                marginTop: 24,
                padding: 14,
                background: COLORS.bone,
                borderRadius: 8,
                fontFamily: mono,
                fontSize: 12,
                color: COLORS.inkSoft,
                opacity: clamp01((f - 120) / 8),
              }}
            >
              <div style={{ marginBottom: 4, color: COLORS.inkGhost }}>Authorization code:</div>
              eyJhbGciOiJSUzI1NiIs… aJxMnPqRsTuVwXyZ_AbCdEf
            </div>
          </BrowserWindow>
        </PainWindow>

        {/* (5) Paste-code terminal — entry 130 */}
        <PainWindow start={130} x={620} y={680} rot={2} scale={0.7} z={5}>
          <TerminalWindow width={780} height={300} title="~ — claude">
            <div style={{ fontSize: 16, color: '#897F70' }}>Paste authorization code:</div>
            <div
              style={{
                fontSize: 16,
                color: COLORS.termText,
                marginTop: 6,
                whiteSpace: 'nowrap',
                overflow: 'hidden',
              }}
            >
              <TypedText
                text="eyJhbGciOiJSUzI1NiIsxK9Lp4m…aJxMnPqRsTuVwXyZ_AbCdEf"
                start={138}
                speedFrames={1}
              />
              {f < 175 && <BlinkCaret height={16} />}
            </div>
            <div
              style={{ marginTop: 12, color: COLORS.green, fontSize: 16, opacity: clamp01((f - 175) / 6) }}
            >
              ✓ Login successful.
            </div>
          </TerminalWindow>
        </PainWindow>

        {/* (6) Sonnet 7-day cap modal — entry 170 */}
        <PainWindow start={170} x={1180} y={520} rot={-1.5} scale={0.8} z={6}>
          <div
            style={{
              width: 600,
              background: '#FFFFFF',
              border: `2.5px solid ${COLORS.red}`,
              borderRadius: 14,
              padding: '22px 26px',
              boxShadow: '0 26px 60px rgba(210,75,58,0.32)',
            }}
          >
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 12,
                fontFamily: sans,
                fontWeight: 700,
                fontSize: 22,
                color: COLORS.red,
                marginBottom: 6,
              }}
            >
              <span
                style={{
                  width: 28,
                  height: 28,
                  borderRadius: 14,
                  background: COLORS.red,
                  color: '#fff',
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  fontSize: 18,
                }}
              >
                !
              </span>
              Sonnet 7-day cap on @team
            </div>
            <div style={{ fontFamily: sans, fontSize: 16, color: COLORS.inkSoft }}>
              Switch to another account, or wait 2 days.
            </div>
          </div>
        </PainWindow>

        {/* (7+) Cascade of small error popups */}
        {cascadeItems.map((it, i) => {
          const t = ease(clamp01((f - it.delay) / 8));
          if (t <= 0) return null;
          return (
            <div
              key={i}
              style={{
                position: 'absolute',
                left: it.x,
                top: it.y,
                opacity: t,
                transform: `scale(${0.55 + 0.2 * t}) rotate(${it.rot}deg)`,
                transformOrigin: 'top left',
                zIndex: 10 + i,
              }}
            >
              <div
                style={{
                  width: 360,
                  background: '#FFFFFF',
                  border: `2px solid ${COLORS.red}`,
                  borderRadius: 12,
                  padding: '14px 18px',
                  boxShadow: '0 18px 40px rgba(210,75,58,0.22)',
                  fontFamily: sans,
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 4 }}>
                  <span
                    style={{
                      width: 18,
                      height: 18,
                      borderRadius: 9,
                      background: COLORS.red,
                      color: '#fff',
                      fontSize: 13,
                      fontWeight: 700,
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                    }}
                  >
                    !
                  </span>
                  <span style={{ color: COLORS.red, fontWeight: 700, fontSize: 16 }}>
                    {it.label}
                  </span>
                </div>
                <div style={{ fontSize: 13, color: COLORS.inkSoft }}>
                  Try again or switch account…
                </div>
              </div>
            </div>
          );
        })}

        {clicks.map((s, i) => (
          <ClickRipple key={i} x={cx + 8} y={cy + 8} start={s} />
        ))}
        <MouseCursor x={cx} y={cy} opacity={cursorOpacity} />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

// ─────────────────────────────────────────────────────────────
// SCENE 2 — IDEA PIVOT (300 → 480 frames, 6s, half the previous)
// "Why do I keep doing this?" → 💡 → "Pool your Claude subscriptions."
// ─────────────────────────────────────────────────────────────
const IdeaPivotScene: React.FC = () => {
  const f = useCurrentFrame();
  const enter = useEnter(0, 10);
  const exit = useExit(180, 12);
  const baseOpacity = Math.min(enter.opacity, exit.opacity);

  // Local 0 → 180 (6s)
  // 0-45: sigh (Why do I keep doing this?)
  // 45-65: short pause/transition
  // 65-100: 💡 + Wait —
  // 100-180: Pool headline lands and holds

  const sighOpacity = clamp01((f - 4) / 8) - clamp01((f - 50) / 8);
  const sighDrift = interpolate(clamp01(f / 50), [0, 1], [0, -8]);

  const ideaT = ease(clamp01((f - 60) / 14));
  const ideaScale = 0.5 + 0.6 * ideaT;
  const ideaOpacity = clamp01((f - 60) / 8) - clamp01((f - 110) / 10);

  const headlineW = (start: number) => clamp01((f - start) / 8);
  const ghostMix = ease(clamp01((f - 140) / 12));
  const r = Math.round(interpolate(ghostMix, [0, 1], [182, 255]));
  const g = Math.round(interpolate(ghostMix, [0, 1], [176, 122]));
  const b = Math.round(interpolate(ghostMix, [0, 1], [164, 26]));
  const underlineGrow = clamp01((f - 148) / 12);
  const underlineFade = clamp01((f - 168) / 10);
  const underlineOpacity = clamp01(underlineGrow - underlineFade);

  return (
    <AbsoluteFill>
      <CreamBackground />
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', opacity: baseOpacity }}>
        <div style={{ position: 'relative', width: 1700, height: 600 }}>
          <div
            style={{
              position: 'absolute',
              left: 0,
              right: 0,
              top: 200,
              textAlign: 'center',
              opacity: sighOpacity,
              transform: `translateY(${sighDrift}px)`,
            }}
          >
            <div
              style={{
                fontFamily: serifItalic,
                fontStyle: 'italic',
                fontSize: 78,
                color: COLORS.inkSoft,
                letterSpacing: -0.6,
              }}
            >
              "Why do I keep doing this?"
            </div>
          </div>
          <div
            style={{
              position: 'absolute',
              left: 0,
              right: 0,
              top: 180,
              textAlign: 'center',
              opacity: ideaOpacity,
              transform: `scale(${ideaScale})`,
            }}
          >
            <div style={{ fontSize: 160, lineHeight: 1, marginBottom: 8 }}>💡</div>
            <div
              style={{
                fontFamily: serif,
                fontSize: 56,
                color: COLORS.orangeDeep,
                fontStyle: 'italic',
              }}
            >
              Wait —
            </div>
          </div>
          <div
            style={{
              position: 'absolute',
              left: 0,
              right: 0,
              top: 220,
              textAlign: 'center',
              fontFamily: serif,
              fontSize: 110,
              color: COLORS.ink,
              letterSpacing: -1.8,
              lineHeight: 1.16,
              opacity: clamp01((f - 110) / 12),
            }}
          >
            <span style={{ opacity: headlineW(110) }}>Pool</span>{' '}
            <span style={{ opacity: headlineW(120) }}>your</span>{' '}
            <span style={{ opacity: headlineW(130) }}>Claude</span>{' '}
            <span
              style={{
                position: 'relative',
                color: `rgb(${r}, ${g}, ${b})`,
                opacity: headlineW(140),
                fontStyle: 'italic',
                display: 'inline-block',
              }}
            >
              subscriptions.
              <span
                style={{
                  position: 'absolute',
                  left: 0,
                  right: 24,
                  bottom: -16,
                  height: 6,
                  background: COLORS.orange,
                  opacity: underlineOpacity,
                  transformOrigin: 'left center',
                  transform: `scaleX(${underlineGrow})`,
                  borderRadius: 3,
                }}
              />
            </span>
          </div>
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};


// ─────────────────────────────────────────────────────────────
// SHARED — Foxy app shell + AccountChip + UsageBar
// ─────────────────────────────────────────────────────────────
const FoxyAppShell: React.FC<{
  children: React.ReactNode;
  activePage?: 'Dashboard' | 'Accounts' | 'Activity' | 'Settings';
}> = ({ children, activePage = 'Dashboard' }) => (
  <div
    style={{
      width: 1500,
      height: 880,
      background: '#FFFFFF',
      borderRadius: 18,
      border: `1px solid ${COLORS.cardBorder}`,
      boxShadow: '0 30px 80px rgba(26,26,26,0.10)',
      overflow: 'hidden',
      display: 'grid',
      gridTemplateColumns: '220px 1fr',
    }}
  >
    <div
      style={{
        background: '#FAF7F1',
        borderRight: `1px solid ${COLORS.cardBorderSoft}`,
        padding: '24px 18px',
        display: 'flex',
        flexDirection: 'column',
        gap: 10,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 22 }}>
        <Img src={staticFile('foxy-icon.png')} style={{ width: 36, height: 36, borderRadius: 8 }} />
        <div style={{ fontFamily: sans, fontSize: 17, fontWeight: 600, color: COLORS.ink }}>
          Foxy Switcher
        </div>
      </div>
      {(['Dashboard', 'Accounts', 'Activity', 'Settings'] as const).map((item) => {
        const active = item === activePage;
        return (
          <div
            key={item}
            style={{
              padding: '10px 14px',
              borderRadius: 10,
              fontFamily: sans,
              fontSize: 15,
              color: active ? COLORS.ink : COLORS.inkSoft,
              background: active ? COLORS.orangeSoft : 'transparent',
              fontWeight: active ? 600 : 400,
            }}
          >
            {item}
          </div>
        );
      })}
      <div
        style={{
          marginTop: 'auto',
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          fontSize: 13,
          color: COLORS.inkSoft,
        }}
      >
        <span style={{ width: 8, height: 8, borderRadius: 4, background: COLORS.green }} />
        <span style={{ fontFamily: sans }}>Daemon · healthy</span>
      </div>
    </div>
    <div style={{ padding: '32px 38px' }}>{children}</div>
  </div>
);

const UsageBar: React.FC<{ label: string; value: number }> = ({ label, value }) => {
  const pct = Math.max(0, Math.min(1, value));
  const color = pct >= 0.95 ? COLORS.orange : pct >= 0.75 ? '#E5A037' : COLORS.green;
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
      <span
        style={{
          fontFamily: sans,
          fontSize: 12,
          color: COLORS.inkSoft,
          width: 64,
          flexShrink: 0,
        }}
      >
        {label}
      </span>
      <div
        style={{
          flex: 1,
          height: 6,
          background: COLORS.cardBorderSoft,
          borderRadius: 3,
          overflow: 'hidden',
        }}
      >
        <div style={{ width: `${pct * 100}%`, height: '100%', background: color }} />
      </div>
      <span
        style={{
          fontFamily: mono,
          fontSize: 12,
          color: COLORS.inkSoft,
          width: 48,
          textAlign: 'right',
          flexShrink: 0,
        }}
      >
        {Math.round(pct * 100)}%
      </span>
    </div>
  );
};

const AccountChip: React.FC<{
  label: string;
  primary?: string;
  morphT?: number;
  status?: 'pending' | 'active' | 'cooldown';
  usage5h?: number;
  usage7d?: number;
  usage7dSon?: number;
  highlight?: boolean;
}> = ({
  label,
  primary,
  morphT = 0,
  status = 'pending',
  usage5h = 0,
  usage7d = 0,
  usage7dSon = 0,
  highlight = false,
}) => {
  const radius = interpolate(morphT, [0, 1], [22, 14]);
  const padding = interpolate(morphT, [0, 1], [10, 22]);
  const showDetails = morphT > 0.6 ? clamp01((morphT - 0.6) / 0.4) : 0;
  const statusColor =
    status === 'active' ? COLORS.green : status === 'cooldown' ? COLORS.orange : COLORS.inkGhost;
  return (
    <div
      style={{
        background: highlight ? COLORS.orangeSoft : '#FFFFFF',
        border: `${highlight ? 2 : 1}px solid ${highlight ? COLORS.orange : COLORS.cardBorderSoft}`,
        borderRadius: radius,
        padding: `${padding}px ${padding * 1.4}px`,
        display: 'flex',
        flexDirection: 'column',
        gap: 12,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <span
          style={{
            width: 10,
            height: 10,
            borderRadius: 5,
            background: statusColor,
            flexShrink: 0,
          }}
        />
        <span style={{ fontFamily: sans, fontSize: 18, fontWeight: 600, color: COLORS.ink }}>
          {label}
        </span>
        {primary && (
          <span
            style={{
              fontFamily: sans,
              fontSize: 11,
              color: COLORS.inkSoft,
              padding: '3px 8px',
              border: `1px solid ${COLORS.cardBorderSoft}`,
              borderRadius: 6,
              marginLeft: 'auto',
            }}
          >
            {primary}
          </span>
        )}
      </div>
      {showDetails > 0 && (
        <div style={{ opacity: showDetails, display: 'flex', flexDirection: 'column', gap: 7 }}>
          <UsageBar label="5h" value={usage5h} />
          <UsageBar label="7d" value={usage7d} />
          <UsageBar label="Sonnet 7d" value={usage7dSon} />
        </div>
      )}
    </div>
  );
};

// ─────────────────────────────────────────────────────────────
// SCENE 3 — ADD ACCOUNTS (960 → 1230 frames, 9s, faster)
// One Add-account click → OAuth flash → 3 chips slide in together → morph to full cards
// ─────────────────────────────────────────────────────────────
const AddAccountsScene: React.FC = () => {
  const f = useCurrentFrame();
  const enter = useEnter(0, 12);
  const exit = useExit(270, 12);
  const baseOpacity = Math.min(enter.opacity, exit.opacity);

  // Local 0 → 270
  // 0-22: shell entrance
  // 22-50: cursor → Add account, click ripple
  // 50-100: OAuth flash
  // 100-150: 3 chips slide in simultaneously
  // 150-220: chips morph to full cards
  // 220-270: hold/zoom slow

  const addBtnX = 1340;
  const addBtnY = 280;

  let cx = 1700,
    cy = 920;
  if (f < 22) {
    cx = 1700;
    cy = 920;
  } else if (f < 60) {
    const t = clamp01((f - 22) / 28);
    cx = interpolate(t, [0, 1], [1700, addBtnX + 60]);
    cy = interpolate(t, [0, 1], [920, addBtnY + 18]);
  } else if (f < 220) {
    cx = addBtnX + 60;
    cy = addBtnY + 18;
  } else {
    const t = clamp01((f - 220) / 50);
    cx = interpolate(t, [0, 1], [addBtnX + 60, 1700]);
    cy = interpolate(t, [0, 1], [addBtnY + 18, 900]);
  }
  const cursorOpacity = f < 240 ? clamp01((f - 6) / 14) : clamp01((270 - f) / 20);

  // OAuth banner: flash 50-100
  const oauthOpacity = clamp01((f - 50) / 6) - clamp01((f - 96) / 6);

  // Chips: slide in 100-150 simultaneously
  const chipT = (idx: number) => ease(clamp01((f - (100 + idx * 6)) / 22));

  // Morph: 150-220
  const morphT = easeInOut(clamp01((f - 150) / 60));

  const shellEnter = clamp01((f - 4) / 14);

  return (
    <AbsoluteFill>
      <GridBackground />
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', opacity: baseOpacity }}>
        <div
          style={{
            opacity: shellEnter,
            transform: `translateY(${(1 - shellEnter) * 30}px) scale(${0.96 + 0.04 * shellEnter})`,
          }}
        >
          <FoxyAppShell activePage="Accounts">
            <div
              style={{
                display: 'flex',
                alignItems: 'baseline',
                justifyContent: 'space-between',
                marginBottom: 24,
              }}
            >
              <div>
                <div
                  style={{
                    fontFamily: sans,
                    fontSize: 26,
                    fontWeight: 700,
                    color: COLORS.ink,
                    letterSpacing: -0.2,
                  }}
                >
                  Accounts
                </div>
                <div style={{ fontFamily: sans, fontSize: 14, color: COLORS.inkSoft, marginTop: 4 }}>
                  Pool any number of Claude subscriptions.
                </div>
              </div>
              <div
                style={{
                  background: COLORS.orange,
                  color: '#fff',
                  fontFamily: sans,
                  fontSize: 15,
                  fontWeight: 600,
                  padding: '12px 22px',
                  borderRadius: 10,
                  boxShadow: '0 4px 14px rgba(255,122,26,0.28)',
                }}
              >
                + Add account
              </div>
            </div>

            {/* OAuth flash */}
            <div
              style={{
                marginBottom: 20,
                opacity: oauthOpacity,
                background: '#FFFFFF',
                border: `1px solid ${COLORS.cardBorderSoft}`,
                borderRadius: 10,
                padding: '14px 18px',
                fontFamily: sans,
                fontSize: 14,
                color: COLORS.inkSoft,
                display: 'flex',
                alignItems: 'center',
                gap: 12,
              }}
            >
              <Spinner size={18} color={COLORS.orange} />
              Browser opened — authorizing 3 Claude accounts…
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 18 }}>
              {[
                { label: '@personal', primary: 'Pro', u5: 0.42, u7: 0.31, us: 0.55, idx: 0 },
                { label: '@team', primary: 'Team', u5: 0.18, u7: 0.42, us: 0.27, idx: 1 },
                { label: '@premium', primary: 'Max', u5: 0.05, u7: 0.20, us: 0.10, idx: 2 },
              ].map((a) => {
                const t = chipT(a.idx);
                return (
                  <div
                    key={a.label}
                    style={{
                      opacity: t,
                      transform: `translateY(${(1 - t) * 24}px)`,
                    }}
                  >
                    <AccountChip
                      label={a.label}
                      primary={a.primary}
                      morphT={morphT}
                      status="pending"
                      usage5h={a.u5}
                      usage7d={a.u7}
                      usage7dSon={a.us}
                    />
                  </div>
                );
              })}
            </div>
          </FoxyAppShell>
        </div>

        <ClickRipple x={cx + 8} y={cy + 8} start={48} />
        <MouseCursor x={cx} y={cy} opacity={cursorOpacity} />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

// ─────────────────────────────────────────────────────────────
// SCENE 4 — AUTO ROTATION (1230 → 1590 frames, 12s, faster)
// Usage fills → 5h cap blinks → @team takes over → credinjector log
// ─────────────────────────────────────────────────────────────
const AutoRotationScene: React.FC = () => {
  const f = useCurrentFrame();
  const enter = useEnter(0, 12);
  const exit = useExit(360, 14);
  const baseOpacity = Math.min(enter.opacity, exit.opacity);

  // Local 0 → 360
  // 0-30: shell entrance
  // 30-130: usage bars fill (faster, 100 frames)
  // 130-160: 5h cap blinks
  // 160-220: @team slides into active slot
  // 220-300: credinjector log line types
  // 300-360: hold

  const fillT = clamp01((f - 30) / 100);
  const personal5h = interpolate(fillT, [0, 1], [0.42, 1.0]);
  const personal7d = interpolate(fillT, [0, 1], [0.31, 0.62]);
  const personalSon = interpolate(fillT, [0, 1], [0.55, 0.84]);

  const capBlink =
    f >= 130 && f < 160 ? (Math.floor((f - 130) / 4) % 2 === 0 ? 1 : 0.3) : f >= 160 ? 1 : 0;

  const personalActive = f < 160;
  const teamActive = f >= 200;

  const logStart = 220;
  const shellEnter = clamp01((f - 4) / 12);

  return (
    <AbsoluteFill>
      <GridBackground />
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', opacity: baseOpacity }}>
        <div
          style={{
            opacity: shellEnter,
            transform: `translateY(${(1 - shellEnter) * 30}px) scale(${0.96 + 0.04 * shellEnter})`,
          }}
        >
          <FoxyAppShell activePage="Dashboard">
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                marginBottom: 22,
              }}
            >
              <div>
                <div
                  style={{
                    fontFamily: sans,
                    fontSize: 26,
                    fontWeight: 700,
                    color: COLORS.ink,
                    letterSpacing: -0.2,
                  }}
                >
                  Dashboard
                </div>
                <div
                  style={{
                    fontFamily: sans,
                    fontSize: 15,
                    color: COLORS.inkSoft,
                    marginTop: 4,
                    display: 'flex',
                    alignItems: 'center',
                    gap: 8,
                  }}
                >
                  <span
                    style={{
                      width: 8,
                      height: 8,
                      borderRadius: 4,
                      background: f < 160 ? COLORS.green : COLORS.orange,
                    }}
                  />
                  Managing{' '}
                  <span style={{ fontFamily: mono, fontWeight: 600, color: COLORS.ink }}>
                    {f < 200 ? '@personal' : '@team'}
                  </span>
                </div>
              </div>
              <div
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 10,
                  fontFamily: sans,
                  fontSize: 14,
                  color: COLORS.inkSoft,
                }}
              >
                Auto Switch
                <div
                  style={{
                    width: 38,
                    height: 22,
                    borderRadius: 11,
                    background: COLORS.orange,
                    position: 'relative',
                  }}
                >
                  <div
                    style={{
                      position: 'absolute',
                      right: 2,
                      top: 2,
                      width: 18,
                      height: 18,
                      borderRadius: 9,
                      background: '#FFFFFF',
                      boxShadow: '0 1px 3px rgba(0,0,0,0.12)',
                    }}
                  />
                </div>
              </div>
            </div>

            <div style={{ display: 'flex', flexDirection: 'column', gap: 12, marginBottom: 22 }}>
              <div>
                <AccountChip
                  label="@personal"
                  primary="Pro"
                  morphT={1}
                  status={personalActive ? 'active' : 'cooldown'}
                  usage5h={f < 160 ? personal5h : 1}
                  usage7d={personal7d}
                  usage7dSon={personalSon}
                  highlight={personalActive}
                />
                {f >= 130 && (
                  <div
                    style={{
                      marginTop: -10,
                      paddingLeft: 22,
                      fontFamily: sans,
                      fontSize: 13,
                      color: COLORS.orange,
                      opacity: capBlink,
                    }}
                  >
                    ⚠ 5h cap reached — cooling down (4h 56m left)
                  </div>
                )}
              </div>
              <div>
                <AccountChip
                  label="@team"
                  primary="Team"
                  morphT={1}
                  status={teamActive ? 'active' : 'pending'}
                  usage5h={0.18}
                  usage7d={0.42}
                  usage7dSon={0.27}
                  highlight={teamActive}
                />
              </div>
              <div>
                <AccountChip
                  label="@premium"
                  primary="Max"
                  morphT={1}
                  status="pending"
                  usage5h={0.05}
                  usage7d={0.20}
                  usage7dSon={0.10}
                />
              </div>
            </div>

            <div
              style={{
                background: '#FAF7F1',
                border: `1px solid ${COLORS.cardBorderSoft}`,
                borderRadius: 10,
                padding: '14px 18px',
                fontFamily: mono,
                fontSize: 14,
                color: COLORS.inkSoft,
                opacity: clamp01((f - logStart) / 14),
              }}
            >
              <span style={{ color: COLORS.green, marginRight: 10 }}>✓</span>
              <TypedText
                text="credinjector: switched ~/.claude/.credentials.json → @team (LRU policy)"
                start={logStart}
                speedFrames={1}
              />
            </div>
          </FoxyAppShell>
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

// ─────────────────────────────────────────────────────────────
// SCENE 5 — CTA OUTRO (1590 → 1830 frames, 8s)
// ─────────────────────────────────────────────────────────────
const CtaOutroScene: React.FC = () => {
  const f = useCurrentFrame();
  const enter = useEnter(0, 18);
  const exit = useExit(240, 18);
  const baseOpacity = Math.min(enter.opacity, exit.opacity);

  const logoT = ease(clamp01(f / 22));
  const w = (start: number) => clamp01((f - start) / 12);

  const ghostMix = ease(clamp01((f - 80) / 16));
  const r = Math.round(interpolate(ghostMix, [0, 1], [182, 255]));
  const g = Math.round(interpolate(ghostMix, [0, 1], [176, 122]));
  const b = Math.round(interpolate(ghostMix, [0, 1], [164, 26]));
  const underlineGrow = clamp01((f - 92) / 14);
  const underlineFade = clamp01((f - 200) / 20);
  const underlineOpacity = clamp01(underlineGrow - underlineFade);

  const githubOpacity = clamp01((f - 90) / 18);
  const footerOpacity = clamp01((f - 130) / 16);

  return (
    <AbsoluteFill>
      <CreamBackground />
      <AbsoluteFill style={{ alignItems: 'center', justifyContent: 'center', opacity: baseOpacity }}>
        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 26 }}>
          <div style={{ opacity: logoT, transform: `scale(${0.85 + 0.15 * logoT})` }}>
            <Img
              src={staticFile('foxy-icon.png')}
              style={{ width: 180, height: 180, borderRadius: 36 }}
            />
          </div>
          <div
            style={{
              fontFamily: serif,
              fontSize: 76,
              color: COLORS.ink,
              letterSpacing: -1.2,
              textAlign: 'center',
              lineHeight: 1.18,
            }}
          >
            <span style={{ opacity: w(30) }}>An</span>{' '}
            <span style={{ opacity: w(38) }}>account</span>{' '}
            <span style={{ opacity: w(46) }}>pool</span>{' '}
            <span style={{ opacity: w(54) }}>for</span>{' '}
            <span style={{ opacity: w(62) }}>Claude</span>{' '}
            <span
              style={{
                position: 'relative',
                color: `rgb(${r}, ${g}, ${b})`,
                opacity: w(70),
                fontStyle: 'italic',
                display: 'inline-block',
              }}
            >
              Code.
              <span
                style={{
                  position: 'absolute',
                  left: 0,
                  right: 14,
                  bottom: -10,
                  height: 5,
                  background: COLORS.orange,
                  opacity: underlineOpacity,
                  transformOrigin: 'left center',
                  transform: `scaleX(${underlineGrow})`,
                  borderRadius: 3,
                }}
              />
            </span>
          </div>
          <div
            style={{
              fontFamily: mono,
              fontSize: 22,
              color: COLORS.inkSoft,
              opacity: githubOpacity,
              marginTop: 12,
            }}
          >
            github.com/hoveychen/foxy-switcher
          </div>
          <div
            style={{
              fontFamily: sans,
              fontSize: 14,
              color: COLORS.inkGhost,
              opacity: footerOpacity,
              letterSpacing: 0.4,
              textTransform: 'uppercase',
            }}
          >
            Crash-safe restore included · macOS-first · open source
          </div>
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

// ─────────────────────────────────────────────────────────────
// MAIN COMPOSITION
// ─────────────────────────────────────────────────────────────
export const FOXY_INTRO_TOTAL_FRAMES = 1350; // 45s @ 30fps

export const FoxyIntro: React.FC = () => (
  <AbsoluteFill>
    <Series>
      <Series.Sequence durationInFrames={300}>
        <PainScenarioScene />
      </Series.Sequence>
      <Series.Sequence durationInFrames={180}>
        <IdeaPivotScene />
      </Series.Sequence>
      <Series.Sequence durationInFrames={270}>
        <AddAccountsScene />
      </Series.Sequence>
      <Series.Sequence durationInFrames={360}>
        <AutoRotationScene />
      </Series.Sequence>
      <Series.Sequence durationInFrames={240}>
        <CtaOutroScene />
      </Series.Sequence>
    </Series>
    {/* BGM — Happy Dance by HoliznaCC0 (CC0). Trimmed to 47s with 0.5s in / 2s out fade. */}
    <Audio src={staticFile('bgm.mp3')} volume={0.6} />
  </AbsoluteFill>
);
