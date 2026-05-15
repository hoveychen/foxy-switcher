import { useCallback, useEffect, useState } from "react";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import {
  type VersionCheckResult,
  checkAppVersion,
  openExternal,
} from "../api";
import { t, tf } from "../i18n";

// localStorage key the Settings toggle reads/writes. "false" disables the
// auto-check on app launch; the menu item and Settings "Check now" button
// still run on demand regardless of the flag.
export const AUTO_UPDATE_CHECK_KEY = "foxy.auto-update-check";

function autoCheckEnabled(): boolean {
  try {
    return localStorage.getItem(AUTO_UPDATE_CHECK_KEY) !== "false";
  } catch {
    return true;
  }
}

// UpdateNotice mounts once near the top of App.tsx. On first paint it (a)
// runs the cached version check if the user hasn't disabled auto-check, and
// (b) listens for `menu:check-updates` events from the macOS Help menu and
// the Settings "Check now" button (re-emitted as the same DOM event). A
// successful check with `has_update=true` reveals the banner; "Later"
// dismisses for this session only — the next launch will surface it again
// if the user hasn't updated yet.
export function UpdateNotice() {
  const [result, setResult] = useState<VersionCheckResult | null>(null);
  const [dismissed, setDismissed] = useState(false);

  const runCheck = useCallback(async (force: boolean) => {
    try {
      const r = await checkAppVersion(force);
      if (r) {
        setResult(r);
        // A fresh check resets the local dismissal — if the user told us
        // to check again, surfacing the result is the whole point.
        if (force) setDismissed(false);
      }
    } catch {
      // network errors are silent — banner just doesn't render
    }
  }, []);

  useEffect(() => {
    if (autoCheckEnabled()) {
      void runCheck(false);
    }
  }, [runCheck]);

  // The macOS Help menu emits `menu:check-updates` (lib.rs handle_menu_event)
  // and the Settings "Check now" button dispatches the same event name on
  // window so both paths feed this single component instead of duplicating
  // banner state in two places.
  useEffect(() => {
    const subs: Promise<UnlistenFn>[] = [
      listen<void>("menu:check-updates", () => {
        void runCheck(true);
      }),
    ];
    const onDom = () => {
      void runCheck(true);
    };
    window.addEventListener("foxy:check-updates", onDom);
    return () => {
      subs.forEach((p) => void p.then((u) => u()).catch(() => {}));
      window.removeEventListener("foxy:check-updates", onDom);
    };
  }, [runCheck]);

  if (!result?.has_update || dismissed) return null;

  return (
    <div className="banner banner-update">
      <div className="banner-body">
        <strong>{t("update.title")}</strong>
        <span className="text-meta">
          {tf("update.available", {
            current: result.current_version,
            latest: result.latest_version,
          })}
        </span>
      </div>
      <div className="banner-actions">
        <button
          type="button"
          className="btn btn-ghost"
          onClick={() => setDismissed(true)}
        >
          {t("update.later")}
        </button>
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => {
            if (result.release_url) {
              void openExternal(result.release_url);
            }
          }}
        >
          {t("update.update_now")}
        </button>
      </div>
    </div>
  );
}
