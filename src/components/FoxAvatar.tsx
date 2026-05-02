type EyeStyle = (props: { cx: number; color: string }) => JSX.Element;
type MouthStyle = (props: { cx: number; cy: number }) => JSX.Element;

const TILE_TONES: Array<{ from: string; to: string; ear: string }> = [
  { from: "#ffe4cc", to: "#ffb785", ear: "#ff7a1a" },
  { from: "#ffe9d6", to: "#ffc89a", ear: "#ff8a3d" },
  { from: "#fff1d6", to: "#ffd699", ear: "#f59e0b" },
  { from: "#ffd9c2", to: "#ff9b6e", ear: "#e2552a" },
  { from: "#fde8e0", to: "#fab39a", ear: "#c8463a" },
  { from: "#fff0e0", to: "#ffc189", ear: "#ff6b1a" },
];

const LED_COLORS: string[] = [
  "#7ce6ec", // cyan
  "#a3f56b", // lime
  "#ff7ae0", // magenta
  "#ffd166", // amber
  "#7ab8ff", // blue
  "#ff8b8b", // coral
];

const EYE_STYLES: EyeStyle[] = [
  // 0: dots
  ({ cx, color }) => <circle cx={cx} cy={17.5} r={1.6} fill={color} />,
  // 1: tall pills
  ({ cx, color }) => (
    <rect x={cx - 1.1} y={15.6} width={2.2} height={4.2} rx={1.1} fill={color} />
  ),
  // 2: wide bars (visor lights)
  ({ cx, color }) => (
    <rect x={cx - 2.2} y={16.8} width={4.4} height={1.4} rx={0.7} fill={color} />
  ),
  // 3: caret ^^
  ({ cx, color }) => (
    <path
      d={`M${cx - 1.8} 18.4 L${cx} 16.4 L${cx + 1.8} 18.4`}
      stroke={color}
      strokeWidth={1.3}
      strokeLinecap="round"
      strokeLinejoin="round"
      fill="none"
    />
  ),
  // 4: ring
  ({ cx, color }) => (
    <circle cx={cx} cy={17.5} r={1.7} stroke={color} strokeWidth={1.1} fill="none" />
  ),
  // 5: cross/X
  ({ cx, color }) => (
    <g
      stroke={color}
      strokeWidth={1.3}
      strokeLinecap="round"
      fill="none"
    >
      <path d={`M${cx - 1.4} 16.1 L${cx + 1.4} 18.9`} />
      <path d={`M${cx + 1.4} 16.1 L${cx - 1.4} 18.9`} />
    </g>
  ),
];

const MOUTH_STYLES: MouthStyle[] = [
  // 0: round dot
  ({ cx, cy }) => <circle cx={cx} cy={cy} r={1.1} fill="#222" />,
  // 1: smile arc
  ({ cx, cy }) => (
    <path
      d={`M${cx - 1.6} ${cy - 0.2} Q${cx} ${cy + 1.4} ${cx + 1.6} ${cy - 0.2}`}
      stroke="#222"
      strokeWidth={1.1}
      strokeLinecap="round"
      fill="none"
    />
  ),
  // 2: flat line
  ({ cx, cy }) => (
    <path
      d={`M${cx - 1.6} ${cy} L${cx + 1.6} ${cy}`}
      stroke="#222"
      strokeWidth={1.2}
      strokeLinecap="round"
    />
  ),
  // 3: small "o"
  ({ cx, cy }) => (
    <ellipse cx={cx} cy={cy} rx={0.9} ry={1.1} fill="#222" />
  ),
];

function hashString(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i += 1) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

function pick<T>(arr: T[], seed: number, salt: number): T {
  return arr[(((seed + Math.imul(salt, 2654435761)) >>> 0) % arr.length)];
}

export function FoxAvatar({
  name,
  size = 40,
  className = "",
}: {
  name: string;
  size?: number;
  className?: string;
}) {
  const seed = hashString(name || "?");
  const tone = pick(TILE_TONES, seed, 1);
  const eye = pick(EYE_STYLES, seed, 2);
  const mouth = pick(MOUTH_STYLES, seed, 3);
  const led = pick(LED_COLORS, seed, 4);
  const radius = Math.round(size * 0.22);
  const gradId = `fox-grad-${seed.toString(36)}`;

  return (
    <span
      className={`fox-avatar ${className}`}
      style={{
        width: size,
        height: size,
        borderRadius: radius,
        background: `linear-gradient(135deg, ${tone.from}, ${tone.to})`,
      }}
      aria-hidden
    >
      <svg
        viewBox="0 0 32 32"
        width={size}
        height={size}
        fill="none"
      >
        <defs>
          <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={tone.ear} stopOpacity="0.95" />
            <stop offset="100%" stopColor={tone.ear} stopOpacity="0.75" />
          </linearGradient>
        </defs>
        {/* Ears */}
        <path d="M5 11 L8 2 L11 10 Z" fill={`url(#${gradId})`} />
        <path d="M6.6 9.6 L8 5 L9.4 9.6 Z" fill="#1f1f1f" />
        <path d="M21 10 L24 2 L27 11 Z" fill={`url(#${gradId})`} />
        <path d="M22.6 9.6 L24 5 L25.4 9.6 Z" fill="#1f1f1f" />
        {/* Visor */}
        <rect x="4" y="13" width="24" height="8.5" rx="3" fill="#1f1f1f" />
        {/* Eyes */}
        {eye({ cx: 11, color: led })}
        {eye({ cx: 21, color: led })}
        {/* Mouth */}
        {mouth({ cx: 16, cy: 25.5 })}
      </svg>
    </span>
  );
}
