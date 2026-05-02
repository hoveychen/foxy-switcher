import { useState } from "react";
import { Topbar } from "../components/Topbar";

type FilterKey = "all" | "switches" | "refreshes" | "errors";

const FILTERS: Array<{ key: FilterKey; label: string }> = [
  { key: "all", label: "All" },
  { key: "switches", label: "Switches" },
  { key: "refreshes", label: "Refreshes" },
  { key: "errors", label: "Errors" },
];

export function ActivityPage({
  autoSwitch,
  onAutoSwitchToggle,
}: {
  autoSwitch: { enabled: boolean; policy: "lru" | "lowest" | "rr" };
  onAutoSwitchToggle: () => void;
}) {
  const [filter, setFilter] = useState<FilterKey>("all");

  return (
    <>
      <Topbar
        title="Activity"
        status={{ label: "Live · waiting for events", tone: "muted" }}
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
      />
      <div className="page">
        <div className="activity-toolbar">
          <div className="filter-chips" role="tablist" aria-label="Event filter">
            {FILTERS.map((f) => (
              <button
                key={f.key}
                type="button"
                role="tab"
                aria-selected={filter === f.key}
                className={`filter-chip ${filter === f.key ? "active" : ""}`}
                onClick={() => setFilter(f.key)}
              >
                {f.label}
              </button>
            ))}
          </div>
        </div>

        <div className="timeline">
          <div className="page-empty">
            <p>No events yet.</p>
            <p className="text-meta">
              The timeline lights up once the daemon exposes{" "}
              <code>GET /api/activity</code> (PRD §5.3, v0.2). Until then,
              credinject switches, token refreshes, and usage polls are written
              to the daemon log only.
            </p>
          </div>
        </div>
      </div>
    </>
  );
}
