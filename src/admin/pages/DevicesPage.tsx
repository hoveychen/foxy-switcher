import { useEffect, useState } from "react";
import { adminApi, AdminApiError, AdminDevice } from "../api";
import { t, tf } from "../../i18n";

interface Props {
  onUnauthorized: () => void;
}

function formatRelative(ts: number): string {
  if (!ts) return t("admin.devices.never");
  const diff = Date.now() - ts;
  if (diff < 60_000) return t("admin.devices.just_now");
  if (diff < 3_600_000) return tf("admin.devices.m_ago", { n: Math.floor(diff / 60_000) });
  if (diff < 86_400_000) return tf("admin.devices.h_ago", { n: Math.floor(diff / 3_600_000) });
  return tf("admin.devices.d_ago", { n: Math.floor(diff / 86_400_000) });
}

// deviceNameMaxLen mirrors the server-side cap so the input can't submit a
// value the backend will 400. Keep in sync with deviceNameMaxLen in
// server/vault/httpserver/api.go.
const deviceNameMaxLen = 64;

// The four provider toggles render identically, so they live in one list
// instead of four hand-copied <label> blocks.
const providerFields = [
  { key: "allow_claude", label: "admin.pair.provider_claude" },
  { key: "allow_codex", label: "admin.pair.provider_codex" },
  { key: "allow_openrouter", label: "admin.pair.provider_openrouter" },
  { key: "allow_deepseek", label: "admin.pair.provider_deepseek" },
] as const;

type ProviderAllowlist = {
  allow_claude: boolean;
  allow_codex: boolean;
  allow_openrouter: boolean;
  allow_deepseek: boolean;
};

export function DevicesPage({ onUnauthorized }: Props) {
  const [devices, setDevices] = useState<AdminDevice[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [togglingId, setTogglingId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingName, setEditingName] = useState("");
  const [savingId, setSavingId] = useState<string | null>(null);

  async function load() {
    setError(null);
    try {
      const r = await adminApi.listDevices();
      setDevices(r.devices ?? []);
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.load"));
    }
  }

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function revoke(d: AdminDevice) {
    if (!confirm(tf("admin.devices.confirm_revoke", { name: d.name }))) {
      return;
    }
    setRevokingId(d.id);
    setError(null);
    try {
      await adminApi.revokeDevice(d.id);
      setDevices((cur) => (cur ? cur.filter((x) => x.id !== d.id) : cur));
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.revoke"));
    } finally {
      setRevokingId(null);
    }
  }

  async function suspend(d: AdminDevice) {
    if (!confirm(tf("admin.devices.confirm_suspend", { name: d.name }))) {
      return;
    }
    setTogglingId(d.id);
    setError(null);
    try {
      await adminApi.suspendDevice(d.id);
      // Mirror the server: token is now dead and its lease is freed, so
      // drop current_lease and stamp disabled_at locally without a reload.
      setDevices((cur) =>
        cur
          ? cur.map((x) =>
              x.id === d.id
                ? { ...x, disabled_at: Date.now(), current_lease: undefined }
                : x,
            )
          : cur,
      );
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.suspend"));
    } finally {
      setTogglingId(null);
    }
  }

  async function resume(d: AdminDevice) {
    setTogglingId(d.id);
    setError(null);
    try {
      await adminApi.resumeDevice(d.id);
      setDevices((cur) =>
        cur ? cur.map((x) => (x.id === d.id ? { ...x, disabled_at: 0 } : x)) : cur,
      );
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.resume"));
    } finally {
      setTogglingId(null);
    }
  }

  // Toggle one provider in a device's allowlist. The vault releases the
  // device's leases so the change takes effect on its next reconcile. For
  // OpenRouter there is no lease — withdrawing the grant revokes the device's
  // derived API key upstream instead, which takes effect immediately. DeepSeek
  // has no lease either, and nothing to revoke upstream: the device's next
  // config sync removes the credential it wrote.
  async function setProviders(d: AdminDevice, next: ProviderAllowlist) {
    setTogglingId(d.id);
    setError(null);
    try {
      await adminApi.setDeviceProviders(
        d.id,
        next.allow_claude,
        next.allow_codex,
        next.allow_openrouter,
        next.allow_deepseek,
      );
      setDevices((cur) =>
        cur
          ? cur.map((x) =>
              x.id === d.id
                ? { ...x, ...next, current_lease: undefined }
                : x,
            )
          : cur,
      );
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.providers"));
    } finally {
      setTogglingId(null);
    }
  }

  function startEdit(d: AdminDevice) {
    setEditingId(d.id);
    setEditingName(d.name);
    setError(null);
  }

  function cancelEdit() {
    setEditingId(null);
    setEditingName("");
  }

  async function saveEdit(d: AdminDevice) {
    const trimmed = editingName.trim();
    if (!trimmed || trimmed === d.name) {
      cancelEdit();
      return;
    }
    setSavingId(d.id);
    setError(null);
    try {
      await adminApi.renameDevice(d.id, trimmed);
      setDevices((cur) =>
        cur ? cur.map((x) => (x.id === d.id ? { ...x, name: trimmed } : x)) : cur,
      );
      cancelEdit();
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      setError(t("admin.devices.error.rename"));
    } finally {
      setSavingId(null);
    }
  }

  return (
    <div className="admin-content">
      <div className="admin-page admin-page--wide">
        <div>
          <h1 className="admin-page__title">{t("admin.devices.title")}</h1>
          <p className="admin-page__subtitle">{t("admin.devices.subtitle")}</p>
        </div>
        {error && <p className="admin-alert admin-alert--error">{error}</p>}
        <div className="admin-card admin-card--table">
          {devices === null ? (
            <p className="admin-table__empty">{t("admin.common.loading")}</p>
          ) : devices.length === 0 ? (
            <p className="admin-table__empty">{t("admin.devices.empty")}</p>
          ) : (
            // The scroller is the last-resort escape hatch: the columns below
            // are merged so the table fits a laptop beside the sidebar, but a
            // long account name or OS string can still push it past the card,
            // and scrolling beats the buttons spilling outside the border.
            <div className="admin-table__scroll">
              <table className="admin-table admin-table--devices">
                <thead>
                  <tr>
                    <th>{t("admin.devices.col.name")}</th>
                    <th>{t("admin.devices.col.current_account")}</th>
                    <th>{t("admin.devices.col.providers")}</th>
                    <th>{t("admin.devices.col.os")}</th>
                    <th>{t("admin.devices.col.last_seen")}</th>
                    <th>{t("admin.devices.col.actions")}</th>
                  </tr>
                </thead>
                <tbody>
                  {devices.map((d) => {
                    const isEditing = editingId === d.id;
                    const isSaving = savingId === d.id;
                    const isSuspended = d.disabled_at !== 0;
                    const isToggling = togglingId === d.id;
                    return (
                      <tr key={d.id}>
                        <td data-label={t("admin.devices.col.name")}>
                          {isEditing ? (
                            <input
                              className="admin-input"
                              value={editingName}
                              onChange={(e) => setEditingName(e.target.value)}
                              onKeyDown={(e) => {
                                if (e.key === "Enter") {
                                  e.preventDefault();
                                  void saveEdit(d);
                                } else if (e.key === "Escape") {
                                  e.preventDefault();
                                  cancelEdit();
                                }
                              }}
                              autoFocus
                              maxLength={deviceNameMaxLen}
                              placeholder={t("admin.devices.rename_placeholder")}
                              disabled={isSaving}
                            />
                          ) : (
                            // One element, not a fragment: the phone layout
                            // makes each cell a two-column grid (label | value),
                            // and loose siblings would each claim their own grid
                            // slot — the hostname would land under the label.
                            <span className="admin-cell">
                              {d.name}
                              {isSuspended && (
                                <span className="admin-badge admin-badge--muted">
                                  {t("admin.devices.status_suspended")}
                                </span>
                              )}
                              {d.hostname && d.hostname !== d.name && (
                                <span className="admin-table__sub">{d.hostname}</span>
                              )}
                            </span>
                          )}
                        </td>
                        <td data-label={t("admin.devices.col.current_account")}>
                          {d.current_lease
                            ? d.current_lease.account_name ||
                              `#${d.current_lease.account_id}`
                            : "—"}
                        </td>
                        <td data-label={t("admin.devices.col.providers")}>
                          <span className="admin-providers admin-providers--grid">
                            {providerFields.map((p) => (
                              <label className="admin-checkbox" key={p.key}>
                                <input
                                  type="checkbox"
                                  checked={d[p.key]}
                                  disabled={isToggling}
                                  onChange={(e) => {
                                    const next: ProviderAllowlist = {
                                      allow_claude: d.allow_claude,
                                      allow_codex: d.allow_codex,
                                      allow_openrouter: d.allow_openrouter,
                                      allow_deepseek: d.allow_deepseek,
                                    };
                                    next[p.key] = e.target.checked;
                                    void setProviders(d, next);
                                  }}
                                />
                                {t(p.label)}
                              </label>
                            ))}
                          </span>
                        </td>
                        {/* OS, arch and app version were three columns of two
                            words each; stacked into one they cost a third of
                            the width and still read as one "what is this box"
                            block. */}
                        <td data-label={t("admin.devices.col.os")}>
                          <span className="admin-cell">
                            {d.os || "—"}
                            {d.os_version ? ` ${d.os_version}` : ""}
                            <span className="admin-table__sub">
                              {[d.arch, d.app_version && `${d.app_version}${d.client_type ? ` (${d.client_type})` : ""}`]
                                .filter(Boolean)
                                .join(" · ") || "—"}
                            </span>
                          </span>
                        </td>
                        <td data-label={t("admin.devices.col.last_seen")}>
                          <span className="admin-cell">
                            {formatRelative(d.last_seen_at)}
                            <span className="admin-table__sub">
                              {tf("admin.devices.paired_ago", { t: formatRelative(d.created_at) })}
                            </span>
                          </span>
                        </td>
                        <td data-label={t("admin.devices.col.actions")}>
                          <span className="admin-actions admin-actions--end">
                            {isEditing ? (
                              <>
                                <button
                                  type="button"
                                  className="admin-button admin-button--primary"
                                  onClick={() => void saveEdit(d)}
                                  disabled={isSaving}
                                  aria-busy={isSaving}
                                >
                                  {isSaving
                                    ? t("admin.devices.renaming")
                                    : t("admin.devices.rename_save")}
                                </button>
                                <button
                                  type="button"
                                  className="admin-button"
                                  onClick={cancelEdit}
                                  disabled={isSaving}
                                >
                                  {t("admin.devices.rename_cancel")}
                                </button>
                              </>
                            ) : (
                              <>
                                <button
                                  type="button"
                                  className="admin-button"
                                  onClick={() => startEdit(d)}
                                  disabled={
                                    editingId !== null || revokingId === d.id || isToggling
                                  }
                                >
                                  {t("admin.devices.rename")}
                                </button>
                                {isSuspended ? (
                                  <button
                                    type="button"
                                    className="admin-button admin-button--primary"
                                    onClick={() => void resume(d)}
                                    disabled={
                                      isToggling || editingId !== null || revokingId === d.id
                                    }
                                    aria-busy={isToggling}
                                  >
                                    {isToggling
                                      ? t("admin.devices.resuming")
                                      : t("admin.devices.resume")}
                                  </button>
                                ) : (
                                  <button
                                    type="button"
                                    className="admin-button"
                                    onClick={() => void suspend(d)}
                                    disabled={
                                      isToggling || editingId !== null || revokingId === d.id
                                    }
                                    aria-busy={isToggling}
                                  >
                                    {isToggling
                                      ? t("admin.devices.suspending")
                                      : t("admin.devices.suspend")}
                                  </button>
                                )}
                                <button
                                  type="button"
                                  className="admin-button admin-button--danger"
                                  onClick={() => revoke(d)}
                                  disabled={
                                    revokingId === d.id || editingId !== null || isToggling
                                  }
                                  aria-busy={revokingId === d.id}
                                >
                                  {revokingId === d.id
                                    ? t("admin.devices.revoking")
                                    : t("admin.devices.revoke")}
                                </button>
                              </>
                            )}
                          </span>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>
        <p className="admin-page__subtitle">
          {t("admin.devices.pair_hint_prefix")}{" "}
          <a className="admin-nav-link" href="/pair">
            {t("admin.nav.pair")}
          </a>
          {t("admin.devices.pair_hint_suffix")}
        </p>
      </div>
    </div>
  );
}
