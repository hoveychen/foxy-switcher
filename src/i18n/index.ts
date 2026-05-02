// Minimal i18n scaffolding. Goals for v0.3:
//
//   1. Keep all user-facing strings discoverable (one JSON file per locale)
//   2. Give us a `t()` call site shape so future translations are a drop-in
//   3. Avoid the bundler tax of a full i18n framework while we have one locale
//
// Expansion plan (post v0.3): add other locales as siblings of `en.json`,
// detect navigator.language at boot, and fall back to en for missing keys.
// `t()`'s signature stays stable — call sites won't need to change.

import en from "./en.json";
import zh from "./zh.json";

type Dict = Record<string, string>;

export type Locale = "auto" | "en" | "zh";

const DICTIONARIES: Record<string, Dict> = { en, zh };
const STORAGE_KEY = "foxy.locale";

function detectSystemLocale(): "en" | "zh" {
  if (typeof navigator === "undefined") return "en";
  const lang = navigator.language?.toLowerCase() ?? "";
  if (lang.startsWith("zh")) return "zh";
  return "en";
}

function readOverride(): Locale {
  if (typeof localStorage === "undefined") return "auto";
  const v = localStorage.getItem(STORAGE_KEY);
  return v === "en" || v === "zh" ? v : "auto";
}

export function getLocaleOverride(): Locale {
  return readOverride();
}

// Persists the user's choice and reloads so every t() call picks up the new
// dictionary. We don't bother with a subscription model — locale flips are
// rare and a reload guarantees no stale renders survive.
export function setLocaleOverride(next: Locale): void {
  if (typeof localStorage === "undefined") return;
  if (next === "auto") localStorage.removeItem(STORAGE_KEY);
  else localStorage.setItem(STORAGE_KEY, next);
  if (typeof window !== "undefined") window.location.reload();
}

const CURRENT = (() => {
  const override = readOverride();
  return override === "auto" ? detectSystemLocale() : override;
})();

function dict(): Dict {
  return DICTIONARIES[CURRENT] ?? en;
}

// t looks up a string key. Missing keys fall back to en, then to the key
// itself (so typoed keys show visibly instead of silently rendering empty)
// and log to the console in dev so we catch them during development.
export function t(key: string): string {
  const v = dict()[key];
  if (v !== undefined) return v;
  const fallback = en[key as keyof typeof en];
  if (fallback !== undefined) return fallback;
  if (import.meta.env.DEV) {
    // eslint-disable-next-line no-console
    console.warn(`[i18n] missing key: ${key}`);
  }
  return key;
}

// tf is the interpolating variant. Variables in the template are written as
// {name} and replaced from the supplied map. Falls back to t()'s key-on-miss
// behavior when the key is absent.
export function tf(key: string, vars: Record<string, string | number>): string {
  const tmpl = t(key);
  return tmpl.replace(/\{(\w+)\}/g, (_, k) =>
    vars[k] !== undefined ? String(vars[k]) : `{${k}}`,
  );
}
