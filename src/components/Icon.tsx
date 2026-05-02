import foxyIconUrl from "../assets/foxy-icon.png";

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
    <img
      src={foxyIconUrl}
      width={size}
      height={size}
      alt=""
      aria-hidden
      draggable={false}
      style={{ display: "block", objectFit: "contain" }}
    />
  );
}
