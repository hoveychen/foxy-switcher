import { useEffect, useState } from "react";
import { Modal } from "./Modal";
import { apiClient } from "../api";
import type { Account, DeepSeekCheck } from "../api";
import { t, tf } from "../i18n";

// DeepSeekModal adds a DeepSeek pool account or rotates its key.
//
// It is deliberately smaller than OpenRouterModal next door, and the
// difference belongs to the provider rather than being a shortcut. DeepSeek
// issues API keys only from its web console: there is no key to mint per
// device, and therefore no model allowlist and no spend cap to configure. An
// account is a name plus a key.
//
// The consequence worth stating in the UI is that every authorised device is
// served this same key — so revoking a device cannot revoke it. That is what
// the "shared" note below says, rather than letting an operator assume a
// per-device revocability that isn't there.
//
// The key never comes back over the API, hence a blank field on edit rather
// than a pre-filled one.

export function DeepSeekModal({
  open,
  account,
  onClose,
  onSaved,
}: {
  open: boolean;
  // null = create a new account; otherwise edit this one.
  account: Account | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const editing = account !== null;
  const [name, setName] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [check, setCheck] = useState<DeepSeekCheck | null>(null);
  const [checking, setChecking] = useState(false);

  // Re-seed whenever the modal opens so a cancelled edit doesn't bleed into
  // the next one.
  useEffect(() => {
    if (!open) return;
    setName(account?.name ?? "");
    setApiKey("");
    setError(null);
    setCheck(null);
  }, [open, account]);

  // Saving validates the key against DeepSeek, so a typo surfaces here rather
  // than at every device's first request. On edit the key is the only editable
  // field, so an empty one means there is nothing to save.
  const canSubmit =
    !busy && apiKey.trim() !== "" && (editing || name.trim() !== "");

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      if (editing && account) {
        await apiClient.updateDeepSeekAccount(account.id, { api_key: apiKey.trim() });
      } else {
        await apiClient.createDeepSeekAccount({
          name: name.trim(),
          api_key: apiKey.trim(),
        });
      }
      onSaved();
      onClose();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  async function runCheck() {
    if (!account) return;
    setChecking(true);
    setError(null);
    try {
      setCheck(await apiClient.checkDeepSeekAccount(account.id));
    } catch (e) {
      setError(String(e));
    } finally {
      setChecking(false);
    }
  }

  const balance = account?.deepseek?.balance;

  return (
    <Modal
      open={open}
      title={t(editing ? "deepseek.title_edit" : "deepseek.title_add")}
      subtitle={t("deepseek.subtitle")}
      onClose={onClose}
      size="md"
      footer={
        <>
          <button className="btn btn-secondary" onClick={onClose} disabled={busy}>
            {t("deepseek.cancel")}
          </button>
          <button className="btn btn-primary" onClick={submit} disabled={!canSubmit}>
            {busy && <span className="spinner" aria-hidden />}
            {t(editing ? "deepseek.save" : "deepseek.create")}
          </button>
        </>
      }
    >
      <div className="settings-card or-form">
        {!editing && (
          <label className="settings-row settings-row-stack">
            <span className="settings-row-label">{t("deepseek.field.name")}</span>
            <input
              className="or-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("deepseek.field.name_placeholder")}
              disabled={busy}
            />
          </label>
        )}

        <label className="settings-row settings-row-stack">
          <span className="settings-row-label">{t("deepseek.field.api_key")}</span>
          <input
            className="or-input"
            type="password"
            autoComplete="off"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={t("deepseek.field.api_key_placeholder")}
            disabled={busy}
          />
          <span className="text-meta or-hint">{t("deepseek.field.api_key_hint")}</span>
          <span className="text-meta or-hint">{t("deepseek.kind.shared")}</span>
        </label>

        {editing && account?.deepseek && (
          <p
            className={`text-meta or-hint ${
              account.deepseek.out_of_balance ? "or-hint-error" : ""
            }`}
          >
            {balance
              ? tf(
                  account.deepseek.out_of_balance
                    ? "deepseek.balance.empty"
                    : "deepseek.balance.remaining",
                  {
                    amount: balance.amount.toFixed(2),
                    currency: balance.currency || "",
                  },
                )
              : t("deepseek.balance.unknown")}
          </p>
        )}

        {editing && (
          <div className="settings-row settings-row-stack">
            <button
              type="button"
              className="btn btn-secondary"
              onClick={runCheck}
              disabled={checking || busy}
            >
              {checking && <span className="spinner" aria-hidden />}
              {t("deepseek.check")}
            </button>
            <span className="text-meta or-hint">{t("deepseek.check_hint")}</span>
            {check && (
              <p
                className={`text-meta or-hint ${
                  check.key_valid ? "or-hint-ok" : "or-hint-error"
                }`}
              >
                {check.key_valid
                  ? tf("deepseek.check.ok", {
                      amount: (check.amount ?? 0).toFixed(2),
                      currency: check.currency || "",
                    })
                  : (check.detail ?? t("deepseek.check.rejected"))}
              </p>
            )}
          </div>
        )}

        {error && <p className="text-meta or-hint or-hint-error">{error}</p>}
      </div>
    </Modal>
  );
}
