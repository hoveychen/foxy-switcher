// Desktop notifications for the few activity events the user genuinely
// cares about while the app is in the background:
//
//   - cred.failed       — credinject couldn't write the keychain
//   - error.*           — anything the daemon flagged as "this should not
//                         have happened silently"
//
// Everything else stays in the Activity page; piping the full firehose
// into the OS notification center would train the user to ignore them.
//
// Permission handshake is lazy: we don't ask until the first eligible
// event arrives. If the user denies, we remember and stop asking for the
// rest of the session.

import {
  isPermissionGranted,
  requestPermission,
  sendNotification,
} from "@tauri-apps/plugin-notification";
import type { ActivityEvent } from "./api";

let permissionState: "unknown" | "granted" | "denied" | "pending" = "unknown";

function shouldNotify(ev: ActivityEvent): boolean {
  if (ev.type === "cred.failed") return true;
  if (ev.type.startsWith("error.")) return true;
  return false;
}

async function ensurePermission(): Promise<boolean> {
  if (permissionState === "granted") return true;
  if (permissionState === "denied") return false;
  if (permissionState === "pending") return false;
  permissionState = "pending";
  try {
    if (await isPermissionGranted()) {
      permissionState = "granted";
      return true;
    }
    const next = await requestPermission();
    permissionState = next === "granted" ? "granted" : "denied";
    return permissionState === "granted";
  } catch {
    // Plugin not available (e.g. Tauri dev with notification permission
    // missing from the capability) — treat as denied so we don't spam the
    // console on every event.
    permissionState = "denied";
    return false;
  }
}

function titleFor(ev: ActivityEvent): string {
  if (ev.type === "cred.failed") return "Credential injection failed";
  if (ev.type.startsWith("error.")) return "Foxy Switcher error";
  return "Foxy Switcher";
}

// notifyForEvents fires one OS notification per eligible event. Callers
// pass only events newer than lastNotifiedID so backlog replays at app
// launch don't trigger a wave of stale alerts.
export async function notifyForEvents(events: ActivityEvent[]): Promise<void> {
  const candidates = events.filter(shouldNotify);
  if (candidates.length === 0) return;
  if (!(await ensurePermission())) return;
  for (const ev of candidates) {
    try {
      sendNotification({ title: titleFor(ev), body: ev.message });
    } catch {
      // Best-effort: swallow per-event errors so one bad payload doesn't
      // poison the rest of the batch.
    }
  }
}
