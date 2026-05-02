export function Icon({
  d,
  size = 16,
  className = "icon",
  strokeWidth = 1.5,
}: {
  d: string;
  size?: number;
  className?: string;
  strokeWidth?: number;
}) {
  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d={d} />
    </svg>
  );
}

export function BrandMark({ size = 14 }: { size?: number }) {
  return (
    <svg
      viewBox="0 0 16 16"
      width={size}
      height={size}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d="M3 5l2-2 3 1.5L11 3l2 2-1 4a4 4 0 0 1-8 0z" />
      <circle cx="6.5" cy="8" r="0.6" fill="currentColor" stroke="none" />
      <circle cx="9.5" cy="8" r="0.6" fill="currentColor" stroke="none" />
    </svg>
  );
}
