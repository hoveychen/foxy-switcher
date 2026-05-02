type Variant = {
  eyes: JSX.Element;
  mouth?: JSX.Element;
};

const FOX_OUTLINE =
  "M3 5l2-2 3 1.5L11 3l2 2-1 4a4 4 0 0 1-8 0z";

const VARIANTS: Variant[] = [
  {
    eyes: (
      <>
        <circle cx="6.4" cy="7.8" r="0.55" fill="currentColor" />
        <circle cx="9.6" cy="7.8" r="0.55" fill="currentColor" />
      </>
    ),
    mouth: <path d="M7.5 9.5q0.5 0.5 1 0" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />,
  },
  {
    eyes: (
      <>
        <path d="M5.8 7.8h1.2" stroke="currentColor" strokeWidth="0.7" strokeLinecap="round" />
        <path d="M9 7.8h1.2" stroke="currentColor" strokeWidth="0.7" strokeLinecap="round" />
      </>
    ),
    mouth: <path d="M7.6 9.6q0.4 0.3 0.8 0" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />,
  },
  {
    eyes: (
      <>
        <circle cx="6.4" cy="7.6" r="0.55" fill="currentColor" />
        <path d="M9 7.8h1.2" stroke="currentColor" strokeWidth="0.7" strokeLinecap="round" />
      </>
    ),
    mouth: <path d="M7.5 9.5q0.5 0.4 1 0.1" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />,
  },
  {
    eyes: (
      <>
        <path d="M5.8 8q0.6-0.6 1.2 0" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />
        <path d="M9 8q0.6-0.6 1.2 0" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />
      </>
    ),
    mouth: <path d="M7.4 9.4q0.6 0.6 1.2 0" stroke="currentColor" strokeWidth="0.6" fill="none" strokeLinecap="round" />,
  },
];

const TONES: Array<{ from: string; to: string }> = [
  { from: "var(--c-orange-400)", to: "var(--c-orange-600)" },
  { from: "var(--c-orange-300)", to: "var(--c-orange-500)" },
  { from: "var(--c-amber-400)", to: "var(--c-orange-600)" },
  { from: "var(--c-orange-500)", to: "var(--c-red-500)" },
];

function pick(seed: number, modulo: number): number {
  return Math.abs(seed) % modulo;
}

export function FoxAvatar({
  id,
  size = 40,
  className = "",
}: {
  id: number;
  size?: number;
  className?: string;
}) {
  const variant = VARIANTS[pick(id, VARIANTS.length)];
  const tone = TONES[pick(id + 1, TONES.length)];
  const radius = Math.round(size * 0.25);
  const inner = Math.round(size * 0.7);

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
        viewBox="0 0 16 16"
        width={inner}
        height={inner}
        fill="none"
        stroke="white"
        strokeWidth="1.4"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d={FOX_OUTLINE} fill="rgba(255,255,255,0.18)" />
        <g stroke="white" color="white">{variant.eyes}</g>
        {variant.mouth}
      </svg>
    </span>
  );
}
