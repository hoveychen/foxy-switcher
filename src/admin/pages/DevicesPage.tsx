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
  // derived API key upstream instead, which takes effect immediately.
  async function setProviders(
    d: AdminDevice,
    next: {
      allow_claude: boolean;
      allow_codex: boolean;
      allow_openrouter: boolean;
    },
  ) {
    setTogglingId(d.id);
    setError(null);
    try {
      await adminApi.setDeviceProviders(
        d.id,
        next.allow_claude,
        next.allow_codex,
        next.allow_openrouter,
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
        <div className="admin-card" style={{ padding: 0 }}>
          {devices === null ? (
            <p className="admin-table__empty">{t("admin.common.loading")}</p>
          ) : devices.length === 0 ? (
            <p className="admin-table__empty">{t("admin.devices.empty")}</p>
          ) : (
            <table className="admin-table">
              <thead>
                <tr>
                  <th>{t("admin.devices.col.name")}</th>
                  <th>{t("admin.devices.col.current_account")}</th>
                  <th>{t("admin.devices.col.providers")}</th>
                  <th>{t("admin.devices.col.os")}</th>
                  <th>{t("admin.devices.col.arch")}</th>
                  <th>{t("admin.devices.col.app")}</th>
                  <th>{t("admin.devices.col.paired")}</th>
                  <th>{t("admin.devices.col.last_seen")}</th>
                  <th></th>
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
                      <td>
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
                          <>
                            {d.name}
                            {isSuspended && (
                              <span className="admin-badge admin-badge--muted">
                                {t("admin.devices.status_suspended")}
                              </span>
                            )}
                            {d.hostname && d.hostname !== d.name && (
                              <span className="admin-table__sub">{d.hostname}</span>
                            )}
                          </>
                        )}
                      </td>
                      <td>
                        {d.current_lease
                          ? d.current_lease.account_name ||
                            `#${d.current_lease.account_id}`
                          : "—"}
                      </td>
                      <td>
                        <span className="admin-providers admin-providers--inline">
                          <label className="admin-checkbox">
                            <input
                              type="checkbox"
                              checked={d.allow_claude}
                              disabled={isToggling}
                              onChange={(e) =>
                                void setProviders(d, {
                                  allow_claude: e.target.checked,
                                  allow_codex: d.allow_codex,
                                  allow_openrouter: d.allow_openrouter,
                                })
                              }
                            />
                            {t("admin.pair.provider_claude")}
                          </label>
                          <label className="admin-checkbox">
                            <input
                              type="checkbox"
                              checked={d.allow_codex}
                              disabled={isToggling}
                              onChange={(e) =>
                                void setProviders(d, {
                                  allow_claude: d.allow_claude,
                                  allow_codex: e.target.checked,
                                  allow_openrouter: d.allow_openrouter,
                                })
                              }
                            />
                            {t("admin.pair.provider_codex")}
                          </label>
                          <label className="admin-checkbox">
                            <input
                              type="checkbox"
                              checked={d.allow_openrouter}
                              disabled={isToggling}
                              onChange={(e) =>
                                void setProviders(d, {
                                  allow_claude: d.allow_claude,
                                  allow_codex: d.allow_codex,
                                  allow_openrouter: e.target.checked,
                                })
                              }
                            />
                            {t("admin.pair.provider_openrouter")}
                          </label>
                        </span>
                      </td>
                      <td>
                        {d.os || "—"}
                        {d.os_version ? ` ${d.os_version}` : ""}
                      </td>
                      <td>{d.arch || "—"}</td>
                      <td>
                        {d.app_version || "—"}
                        {d.client_type ? ` (${d.client_type})` : ""}
                      </td>
                      <td>{formatRelative(d.created_at)}</td>
                      <td>{formatRelative(d.last_seen_at)}</td>
                      <td>
                        <span className="admin-actions" style={{ justifyContent: "flex-end" }}>
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
