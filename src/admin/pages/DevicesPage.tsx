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

export function DevicesPage({ onUnauthorized }: Props) {
  const [devices, setDevices] = useState<AdminDevice[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);

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

  return (
    <div className="admin-content">
      <div className="admin-page">
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
                  <th>{t("admin.devices.col.os")}</th>
                  <th>{t("admin.devices.col.arch")}</th>
                  <th>{t("admin.devices.col.app")}</th>
                  <th>{t("admin.devices.col.paired")}</th>
                  <th>{t("admin.devices.col.last_seen")}</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {devices.map((d) => (
                  <tr key={d.id}>
                    <td>
                      {d.name}
                      {d.hostname && d.hostname !== d.name && (
                        <span className="admin-table__sub">{d.hostname}</span>
                      )}
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
                      <button
                        type="button"
                        className="admin-button admin-button--danger"
                        onClick={() => revoke(d)}
                        disabled={revokingId === d.id}
                        aria-busy={revokingId === d.id}
                      >
                        {revokingId === d.id
                          ? t("admin.devices.revoking")
                          : t("admin.devices.revoke")}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
        <p className="admin-page__subtitle">
          {t("admin.devices.pair_hint_prefix")}{" "}
          <a className="admin-nav-link" href="/admin/pair">
            {t("admin.nav.pair")}
          </a>
          {t("admin.devices.pair_hint_suffix")}
        </p>
      </div>
    </div>
  );
}
