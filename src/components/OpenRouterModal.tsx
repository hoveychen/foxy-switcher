import { useEffect, useState } from "react";
import { Modal } from "./Modal";
import { apiClient } from "../api";
import type { Account, OpenRouterCapabilities } from "../api";
import { t, tf } from "../i18n";

// OpenRouterModal adds or retunes an OpenRouter pool account.
//
// There is no OAuth dance here, unlike Claude and Codex: OpenRouter accounts
// are configured, not signed in to. The admin supplies a management key plus a
// policy (which models, how much spend), and each authorised device later
// derives its own capped runtime key from it. The management key never leaves
// the vault, and never comes back over the API either — hence "leave blank to
// keep" on edit rather than a pre-filled field.
//
// The model allowlist is the single source of truth for two things at once: the
// upstream guardrail that enforces it server-side, and which model profiles a
// device writes for codex. That is why editing it is a heavier action than it
// looks — every key already derived from this account is revoked, because each
// carries the old policy baked into its own guardrail.

const LIMIT_RESETS = ["", "daily", "weekly", "monthly"] as const;

export function OpenRouterModal({
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
  const [models, setModels] = useState("");
  const [limitUSD, setLimitUSD] = useState("");
  const [limitReset, setLimitReset] = useState("");
  const [workspaceID, setWorkspaceID] = useState("");
  const [managementKey, setManagementKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [caps, setCaps] = useState<OpenRouterCapabilities | null>(null);
  const [checking, setChecking] = useState(false);

  // Re-seed whenever the modal opens so a cancelled edit doesn't bleed into the
  // next one.
  useEffect(() => {
    if (!open) return;
    const cfg = account?.openrouter;
    setName(account?.name ?? "");
    setModels((cfg?.allowed_models ?? []).join("\n"));
    setLimitUSD(cfg?.limit_usd ? String(cfg.limit_usd) : "");
    setLimitReset(cfg?.limit_reset ?? "");
    setWorkspaceID(cfg?.workspace_id ?? "");
    setManagementKey("");
    setError(null);
    setCaps(null);
  }, [open, account]);

  const parsedModels = models
    .split(/[\n,]/)
    .map((m) => m.trim())
    .filter(Boolean);
  const parsedLimit = limitUSD.trim() === "" ? 0 : Number(limitUSD);
  const limitInvalid = Number.isNaN(parsedLimit) || parsedLimit < 0;
  // OpenRouter cannot express a never-resetting budget: a guardrail carrying
  // limit_usd must also carry reset_interval. Block the combination here rather
  // than letting the save 400 — the two fields are right next to each other, so
  // the fix is obvious in place.
  const resetMissing = parsedLimit > 0 && limitReset === "";
  const derivedKeys = account?.openrouter?.derived_key_count ?? 0;

  const canSubmit =
    !busy &&
    !limitInvalid &&
    !resetMissing &&
    parsedModels.length > 0 &&
    (editing ? true : name.trim() !== "" && managementKey.trim() !== "");

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      const body = {
        allowed_models: parsedModels,
        limit_usd: parsedLimit,
        limit_reset: limitReset,
        workspace_id: workspaceID.trim(),
      };
      if (editing && account) {
        await apiClient.updateOpenRouterAccount(account.id, {
          ...body,
          // Blank means "keep the stored key" — the API never hands it back, so
          // sending an empty string is the only way to say "unchanged".
          ...(managementKey.trim() ? { management_key: managementKey.trim() } : {}),
        });
      } else {
        await apiClient.createOpenRouterAccount({
          ...body,
          name: name.trim(),
          management_key: managementKey.trim(),
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

  // The capability probe answers the one question the whole design hinges on:
  // does this account's plan actually enforce the model allowlist server-side,
  // or is it only advisory? Surfacing it here means an operator can find out
  // without reading logs.
  async function check() {
    if (!account) return;
    setChecking(true);
    setError(null);
    try {
      setCaps(await apiClient.checkOpenRouterAccount(account.id));
    } catch (e) {
      setError(String(e));
    } finally {
      setChecking(false);
    }
  }

  return (
    <Modal
      open={open}
      title={t(editing ? "openrouter.title_edit" : "openrouter.title_add")}
      subtitle={t("openrouter.subtitle")}
      onClose={onClose}
      size="lg"
      footer={
        <>
          <button className="btn btn-secondary" onClick={onClose} disabled={busy}>
            {t("openrouter.cancel")}
          </button>
          <button className="btn btn-primary" onClick={submit} disabled={!canSubmit}>
            {busy && <span className="spinner" aria-hidden />}
            {t(editing ? "openrouter.save" : "openrouter.create")}
          </button>
        </>
      }
    >
      <div className="settings-card or-form">
        {!editing && (
          <label className="settings-row settings-row-stack">
            <span className="settings-row-label">{t("openrouter.field.name")}</span>
            <input
              className="or-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("openrouter.field.name_placeholder")}
              disabled={busy}
            />
          </label>
        )}

        <label className="settings-row settings-row-stack">
          <span className="settings-row-label">{t("openrouter.field.models")}</span>
          <textarea
            className="or-input"
            rows={5}
            value={models}
            onChange={(e) => setModels(e.target.value)}
            placeholder={t("openrouter.field.models_placeholder")}
            disabled={busy}
          />
          <span className="text-meta or-hint">{t("openrouter.field.models_hint")}</span>
        </label>

        <div className="settings-row or-form-row">
          <label className="or-field">
            <span className="settings-row-label">{t("openrouter.field.limit")}</span>
            <input
              className="or-input"
              inputMode="decimal"
              value={limitUSD}
              onChange={(e) => setLimitUSD(e.target.value)}
              placeholder="25"
              disabled={busy}
            />
            {limitInvalid && (
              <span className="text-meta or-hint or-hint-error">
                {t("openrouter.field.limit_invalid")}
              </span>
            )}
          </label>
          <label className="or-field">
            <span className="settings-row-label">{t("openrouter.field.limit_reset")}</span>
            <select
              className="or-input"
              value={limitReset}
              onChange={(e) => setLimitReset(e.target.value)}
              disabled={busy}
            >
              {LIMIT_RESETS.map((v) => (
                <option key={v || "lifetime"} value={v} disabled={v === "" && parsedLimit > 0}>
                  {t(`openrouter.reset.${v || "lifetime"}`)}
                </option>
              ))}
            </select>
            {resetMissing && (
              <span className="text-meta or-hint or-hint-error">
                {t("openrouter.field.reset_required")}
              </span>
            )}
          </label>
        </div>

        <label className="settings-row settings-row-stack">
          <span className="settings-row-label">{t("openrouter.field.workspace")}</span>
          <input
            className="or-input"
            value={workspaceID}
            onChange={(e) => setWorkspaceID(e.target.value)}
            placeholder={t("openrouter.field.workspace_placeholder")}
            disabled={busy}
          />
        </label>

        <label className="settings-row settings-row-stack">
          <span className="settings-row-label">{t("openrouter.field.management_key")}</span>
          <input
            className="or-input"
            type="password"
            autoComplete="off"
            value={managementKey}
            onChange={(e) => setManagementKey(e.target.value)}
            placeholder={t(
              editing && account?.openrouter?.has_management_key
                ? "openrouter.field.management_key_keep"
                : "openrouter.field.management_key_placeholder",
            )}
            disabled={busy}
          />
          <span className="text-meta or-hint">{t("openrouter.field.management_key_hint")}</span>
        </label>

        {editing && derivedKeys > 0 && (
          <p className="text-meta or-hint or-hint-warn">
            {tf("openrouter.revoke_warning", { count: derivedKeys })}
          </p>
        )}

        {editing && (
          <div className="settings-row settings-row-stack">
            <button
              type="button"
              className="btn btn-secondary"
              onClick={check}
              disabled={checking || busy}
            >
              {checking && <span className="spinner" aria-hidden />}
              {t("openrouter.check")}
            </button>
            <span className="text-meta or-hint">{t("openrouter.check_hint")}</span>
            {caps && (
              <p
                className={`text-meta or-hint ${
                  caps.management_key_valid && caps.guardrails_available
                    ? "or-hint-ok"
                    : "or-hint-warn"
                }`}
              >
                {caps.detail}
              </p>
            )}
          </div>
        )}

        {error && <p className="text-meta or-hint or-hint-error">{error}</p>}
      </div>
    </Modal>
  );
}
