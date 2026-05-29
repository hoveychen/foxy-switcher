import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Channel, invoke } from "@tauri-apps/api/core";
import { Topbar } from "../components/Topbar";
import { apiClient, getServerPort, isTauriHost, type ActivityEvent } from "../api";
import { t, tf } from "../i18n";

// ActivityStreamMsg mirrors the Rust StreamMsg enum (apiproxy.rs). The desktop
// stream arrives over a Tauri Channel instead of a native EventSource so the
// loopback connection isn't subject to the system proxy; these are the
// onopen / onmessage / onerror pendants.
type ActivityStreamMsg =
  | { kind: "open" }
  | { kind: "event"; data: string }
  | { kind: "closed" };

type FilterKey = "all" | "switches" | "refreshes" | "errors";

const FILTERS: Array<{ key: FilterKey; labelKey: string; types?: string }> = [
  { key: "all", labelKey: "activity.filters.all" },
  // "Switches" surfaces both directions of credential ownership transfer so
  // the user sees a paired (injected → restored) story rather than half of it.
  { key: "switches", labelKey: "activity.filters.switches", types: "cred.injected,cred.restored,cred.failed" },
  { key: "refreshes", labelKey: "activity.filters.refreshes", types: "token.refreshed,usage.polled" },
  { key: "errors", labelKey: "activity.filters.errors", types: "error.*" },
];

function fmtTime(ms: number): string {
  const d = new Date(ms);
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function fmtRelative(ms: number, now: number): string {
  const diff = Math.max(0, now - ms);
  const s = Math.floor(diff / 1000);
  if (s < 60) return tf("activity.relative.s", { n: s });
  const m = Math.floor(s / 60);
  if (m < 60) return tf("activity.relative.m", { n: m });
  const h = Math.floor(m / 60);
  if (h < 24) return tf("activity.relative.h", { n: h });
  const d = Math.floor(h / 24);
  return tf("activity.relative.d", { n: d });
}

// Poll fallback when SSE is unavailable (older webview, daemon down, network
// hiccup). 4s matches the original cadence — it's a backstop, not the
// happy path.
const POLL_INTERVAL_MS = 4000;
const MAX_EVENTS = 500;

// Event types that pass the active filter chip. When `types` is undefined
// (the "All" chip), every event matches.
function eventMatchesFilter(ev: ActivityEvent, types?: string): boolean {
  if (!types) return true;
  for (const raw of types.split(",")) {
    const t = raw.trim();
    if (!t) continue;
    if (t.endsWith(".*")) {
      if (ev.type.startsWith(t.slice(0, -1))) return true;
    } else if (ev.type === t) {
      return true;
    }
  }
  return false;
}

export function ActivityPage({
  autoSwitch,
  onAutoSwitchToggle,
}: {
  autoSwitch?: { enabled: boolean; policy: "lru" | "lowest" | "rr" };
  onAutoSwitchToggle?: () => void;
}) {
  const [filter, setFilter] = useState<FilterKey>("all");
  const [events, setEvents] = useState<ActivityEvent[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState<number>(Date.now());
  // "live" → SSE attached, "fallback" → polling, "down" → both failed.
  const [streamState, setStreamState] = useState<"live" | "fallback" | "down">(
    "fallback",
  );
  const [paused, setPaused] = useState(false);
  // Buffer events that arrive while paused so the user can flush them with
  // the "Resume" button without losing anything.
  const [pendingCount, setPendingCount] = useState(0);
  const pendingRef = useRef<ActivityEvent[]>([]);
  const listRef = useRef<HTMLOListElement | null>(null);

  const activeFilter = useMemo(
    () => FILTERS.find((f) => f.key === filter) ?? FILTERS[0],
    [filter],
  );

  const fetchEvents = useCallback(async () => {
    try {
      const data = await apiClient.listActivity({
        limit: MAX_EVENTS,
        type: activeFilter.types,
      });
      setEvents(data);
      setError(null);
      return true;
    } catch (e) {
      setError(String(e));
      return false;
    } finally {
      setLoaded(true);
    }
  }, [activeFilter.types]);

  // SSE / polling lifecycle. We always do an initial fetch (cheap, primes
  // the list with the backlog), then attempt to attach an EventSource. If
  // the EventSource fails fast we drop to polling.
  useEffect(() => {
    setLoaded(false);
    setEvents([]);
    pendingRef.current = [];
    setPendingCount(0);

    let cancelled = false;
    let es: EventSource | null = null;
    let streamId: number | null = null;
    let pollTimer: ReturnType<typeof setInterval> | null = null;

    const startPolling = () => {
      if (pollTimer) return;
      setStreamState((s) => (s === "live" ? "fallback" : s));
      pollTimer = setInterval(() => {
        void fetchEvents();
      }, POLL_INTERVAL_MS);
    };

    const ingest = (ev: ActivityEvent) => {
      if (!eventMatchesFilter(ev, activeFilter.types)) return;
      if (paused) {
        pendingRef.current.push(ev);
        setPendingCount(pendingRef.current.length);
        return;
      }
      setEvents((prev) => {
        // Server returns newest-first from /api/activity, and SSE events
        // arrive in chronological order. Keep the list newest-first to match
        // the bootstrap shape.
        const next = [ev, ...prev];
        if (next.length > MAX_EVENTS) next.length = MAX_EVENTS;
        return next;
      });
    };

    (async () => {
      const ok = await fetchEvents();
      if (cancelled) return;
      if (!ok) {
        startPolling();
        return;
      }

      const handleOpen = () => {
        if (cancelled) return;
        setStreamState("live");
        // Cancel any polling we started during reconnect.
        if (pollTimer) {
          clearInterval(pollTimer);
          pollTimer = null;
        }
      };
      const handleError = () => {
        if (cancelled) return;
        setStreamState((s) => (s === "live" ? "fallback" : "down"));
        startPolling();
      };

      // Desktop: stream over the Rust IPC channel so the loopback connection
      // bypasses the system proxy (see apiproxy.rs). The Go SSE handler is
      // parsed Rust-side; we just get open / event / closed messages here.
      if (isTauriHost()) {
        const channel = new Channel<ActivityStreamMsg>();
        channel.onmessage = (msg) => {
          if (cancelled) return;
          if (msg.kind === "open") {
            handleOpen();
          } else if (msg.kind === "event") {
            try {
              ingest(JSON.parse(msg.data) as ActivityEvent);
            } catch {
              // ignore malformed payload
            }
          } else {
            // "closed": the Rust task ended (daemon restart / network). Unlike
            // EventSource there's no built-in retry, so polling carries us
            // until this effect re-runs and reattaches the stream.
            handleError();
          }
        };
        try {
          streamId = await invoke<number>("activity_stream", { onEvent: channel });
        } catch {
          startPolling();
          return;
        }
        // Raced with unmount between await points — stop the task we just
        // started so it doesn't leak.
        if (cancelled && streamId !== null) {
          void invoke("activity_stream_stop", { id: streamId });
          streamId = null;
        }
        return;
      }

      // Browser / vault mode: native EventSource. There's no local sidecar on
      // the user's 127.0.0.1 here, so this typically fails fast and the
      // polling fallback (same-origin api()) takes over — unchanged behaviour.
      let port: number;
      try {
        port = await getServerPort();
      } catch {
        startPolling();
        return;
      }
      if (cancelled) return;

      try {
        es = new EventSource(`http://127.0.0.1:${port}/api/activity/stream`);
      } catch {
        startPolling();
        return;
      }

      es.addEventListener("activity", (e) => {
        try {
          const data = JSON.parse((e as MessageEvent).data) as ActivityEvent;
          ingest(data);
        } catch {
          // ignore malformed payload
        }
      });

      es.onopen = handleOpen;
      es.onerror = handleError;
    })();

    return () => {
      cancelled = true;
      if (es) es.close();
      if (streamId !== null) void invoke("activity_stream_stop", { id: streamId });
      if (pollTimer) clearInterval(pollTimer);
    };
    // `paused` intentionally excluded — flipping pause shouldn't reconnect
    // the stream; ingest reads the latest value via closure each call.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeFilter.types, fetchEvents]);

  // Pause toggle: when resuming, drain the buffered events into the list.
  const togglePause = () => {
    if (paused && pendingRef.current.length > 0) {
      const drained = pendingRef.current.slice().reverse(); // chronological
      pendingRef.current = [];
      setPendingCount(0);
      setEvents((prev) => {
        const next = [...drained, ...prev];
        if (next.length > MAX_EVENTS) next.length = MAX_EVENTS;
        return next;
      });
    }
    setPaused((p) => !p);
  };

  // Tick the relative-time clock so "5s ago" doesn't sit there for a minute.
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const eventsLabel =
    events.length === 1
      ? t("activity.status.events_one")
      : tf("activity.status.events_many", { count: events.length });
  const status = error
    ? { label: t("activity.status.error"), tone: "danger" as const }
    : streamState === "down"
      ? { label: t("activity.status.disconnected"), tone: "danger" as const }
      : loaded && events.length === 0
        ? { label: t("activity.status.no_events"), tone: "muted" as const }
        : {
            label: `${eventsLabel}${
              streamState === "live" ? t("activity.status.live_suffix") : ""
            }`,
            tone: "ok" as const,
          };

  return (
    <>
      <Topbar
        title={t("activity.title")}
        status={status}
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
      />
      <div className="page">
        <div className="activity-toolbar">
          <div className="filter-chips" role="tablist" aria-label={t("activity.filters.aria")}>
            {FILTERS.map((f) => (
              <button
                key={f.key}
                type="button"
                role="tab"
                aria-selected={filter === f.key}
                className={`filter-chip ${filter === f.key ? "active" : ""}`}
                onClick={() => setFilter(f.key)}
              >
                {t(f.labelKey)}
              </button>
            ))}
          </div>
          <button
            type="button"
            className={`btn ${paused ? "btn-warn" : ""}`}
            onClick={togglePause}
            title={
              paused
                ? tf("activity.resume.title", { count: pendingCount })
                : t("activity.pause.title")
            }
          >
            {paused
              ? pendingCount > 0
                ? tf("activity.resume_with_count", { count: pendingCount })
                : t("activity.resume")
              : t("activity.pause")}
          </button>
        </div>

        <div className="timeline">
          {error ? (
            <div className="page-empty">
              <p>{error}</p>
            </div>
          ) : !loaded ? (
            <div className="page-empty">
              <p className="text-meta">{t("common.loading")}</p>
            </div>
          ) : events.length === 0 ? (
            <div className="page-empty">
              <p>{t("common.no_events_match")}</p>
            </div>
          ) : (
            <ol className="timeline-list" ref={listRef}>
              {events.map((ev) => (
                <li key={ev.id} className={`timeline-item sev-${ev.severity}`}>
                  <span
                    className={`timeline-dot sev-${ev.severity}`}
                    aria-hidden
                  />
                  <div className="timeline-body">
                    <div className="timeline-row">
                      <span className="timeline-type">{ev.type}</span>
                      <span className="text-meta timeline-when">
                        {fmtRelative(ev.timestamp, now)}
                      </span>
                    </div>
                    <div className="timeline-message">{ev.message}</div>
                    <div className="text-meta timeline-meta">
                      {fmtTime(ev.timestamp)}
                      {ev.account_id
                        ? tf("activity.account_suffix", { id: ev.account_id })
                        : ""}
                    </div>
                  </div>
                </li>
              ))}
            </ol>
          )}
        </div>
      </div>
    </>
  );
}
