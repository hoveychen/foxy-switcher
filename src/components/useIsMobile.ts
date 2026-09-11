import { useEffect, useState } from "react";

// MOBILE_QUERY mirrors the 767px breakpoint shell.css uses to flip the
// sidebar into a bottom tab bar. Two places need the phone layout in
// JS rather than CSS — the nav's "More" sheet and the accounts page's
// add-account menu — because both collapse several targets into one
// that needs open/closed state. Keeping the number here means the
// breakpoint is stated twice (here and in shell.css), not five times.
const MOBILE_QUERY = "(max-width: 767px)";

export function useIsMobile(): boolean {
  const [isMobile, setIsMobile] = useState(() => {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia(MOBILE_QUERY).matches;
  });
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia(MOBILE_QUERY);
    const onChange = () => setIsMobile(mq.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return isMobile;
}
