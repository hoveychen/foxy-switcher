import { useCallback, useEffect, useRef, useState } from "react";
import { Icon } from "../components/Icon";
import { ICON_X } from "../components/icons";
import { t } from "../i18n";
import { FoxyIntroPlayer } from "./FoxyIntroPlayer";
import {
  apiClient,
  getDeviceInfo,
  restartDaemon,
  saveAgentConfig,
} from "../api";

// OnboardingOverlay walks new users through the vault choice before
// letting them into the app. Phases:
//   intro       — 45s product video. Skip → "choose".
//   choose      — local / cloud picker. Local → onDismiss().
//                 Cloud → "cloud-input".
//   cloud-input — vault URL + deploy-docs link. Continue → kicks off
//                 pair-init and moves to "cloud-pair".
//   cloud-pair  — show user_code and verification URL, poll until the
//                 vault marks the device approved. Approval saves the
//                 token and restarts the daemon (so it boots in
//                 modeAgent next time per detectModeFromConfig); any
//                 failure (network / denied / expired) drops back to
//                 "cloud-input" with the URL preserved and an inline
//                 error message — never an "are you stuck forever"
//                 dead-end screen.
//
// The overlay is a hard gate: there is no top-right "X" once the user
// has finished the intro video. App.tsx pre-empts this overlay for
// users who already have a working vault (paired agent or combined
// mode with existing accounts) so upgrades don't re-prompt.
type Phase = "intro" | "choose" | "cloud-input" | "cloud-pair";

const DEPLOY_DOCS_URL = "https://hoveychen.github.io/foxy-switcher/deploy.html";

export function OnboardingOverlay({ onDismiss }: { onDismiss: () => void }) {
  const [phase, setPhase] = useState<Phase>("intro");
  const [vaultUrl, setVaultUrl] = useState("");
  const [errorMsg, setErrorMsg] = useState("");
  const [userCode, setUserCode] = useState("");
  const [verificationUrl, setVerificationUrl] = useState("");
  const cancelRef = useRef<{ canceled: boolean }>({ canceled: false });

  const stripTrailingSlash = (s: string) => s.replace(/\/+$/, "");

  // Cloud pair flow. PairVaultModal does the same thing for the
  // Settings entry point; we deliberately don't call into it because
  // the wizard needs different "what to do on failure" semantics
  // (return to URL input, not a dead-end Retry button).
  const startCloudPair = useCallback(async (rawUrl: string) => {
    const url = stripTrailingSlash(rawUrl.trim());
    if (!url) {
      setErrorMsg(t("pair.error.url_required"));
      return;
    }
    setErrorMsg("");
    cancelRef.current = { canceled: false };
    const clientNonce = `nonce-${Math.random().toString(36).slice(2)}-${Date.now()}`;
    const meta = await getDeviceInfo();
    const deviceName =
      meta?.hostname ||
      (typeof navigator !== "undefined" && navigator.platform
        ? `Foxy ${navigator.platform}`
        : "Foxy device");
    let init;
    try {
      init = await apiClient.pairInit(url, deviceName, clientNonce, meta);
    } catch (e) {
      setErrorMsg(
        `${t("onboarding.cloud.pair.failure")} (${e instanceof Error ? e.message : String(e)})`,
      );
      return;
    }
    setUserCode(init.user_code);
    setVerificationUrl(init.verification_url);
    setPhase("cloud-pair");

    // Poll loop. apiClient.pairPoll returns terminal device-flow
    // outcomes as { status: "denied" | "expired" } rather than
    // throwing, so transient throws here mean network blips and we
    // retry until the deadline.
    const deadline = Date.now() + init.expires_in_ms;
    while (!cancelRef.current.canceled && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 2000));
      if (cancelRef.current.canceled) return;
      try {
        const out = await apiClient.pairPoll(url, clientNonce);
        if (out.status === "approved" && out.device_token && out.device_id) {
          try {
            await saveAgentConfig({
              vault_url: url,
              device_id: out.device_id,
              device_token: out.device_token,
            });
            // Restart so detectModeFromConfig promotes the daemon to
            // modeAgent on the next launch. The cachedPort is updated
            // in api.ts so the next api() call hits the new sidecar.
            await restartDaemon();
          } catch (e) {
            setErrorMsg(
              `${t("pair.error.save_failed")}: ${e instanceof Error ? e.message : String(e)}`,
            );
            setPhase("cloud-input");
            return;
          }
          onDismiss();
          return;
        }
        if (out.status === "denied") {
          setErrorMsg(t("pair.error.denied"));
          setPhase("cloud-input");
          return;
        }
        if (out.status === "expired") {
          setErrorMsg(t("pair.error.expired"));
          setPhase("cloud-input");
          return;
        }
      } catch {
        // Transient — keep polling until deadline.
      }
    }
    if (!cancelRef.current.canceled) {
      setErrorMsg(t("pair.error.expired"));
      setPhase("cloud-input");
    }
  }, [onDismiss]);

  const cancelCloudPair = useCallback(() => {
    cancelRef.current.canceled = true;
    setPhase("cloud-input");
  }, []);

  // Esc only escapes the intro video (matching the previous behaviour
  // where Skip dismissed the overlay entirely). After intro the user
  // must complete the vault choice — no Esc shortcut.
  useEffect(() => {
    if (phase !== "intro") return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setPhase("choose");
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [phase]);

  if (phase === "intro") {
    return (
      <div
        className="onboarding-backdrop"
        role="dialog"
        aria-modal="true"
        aria-label={t("onboarding.title")}
      >
        <div className="onboarding-frame">
          <button
            type="button"
            className="onboarding-skip"
            onClick={() => setPhase("choose")}
            aria-label={t("onboarding.skip")}
          >
            <span>{t("onboarding.skip")}</span>
            <Icon d={ICON_X} />
          </button>
          <FoxyIntroPlayer
            autoPlay
            loop={false}
            controls
            clickToPlay
            spaceKeyToPlayOrPause
            onEnded={() => setPhase("choose")}
          />
        </div>
      </div>
    );
  }

  return (
    <div
      className="onboarding-backdrop"
      role="dialog"
      aria-modal="true"
      aria-label={t("onboarding.choose.title")}
    >
      <div className="onboarding-wizard">
        {phase === "choose" && (
          <>
            <h2 className="onboarding-wizard-title">
              {t("onboarding.choose.title")}
            </h2>
            <p className="onboarding-wizard-body">
              {t("onboarding.choose.body")}
            </p>
            <div className="onboarding-choice-grid">
              <button
                type="button"
                className="onboarding-choice"
                onClick={onDismiss}
              >
                <span className="onboarding-choice-title">
                  {t("onboarding.choose.local.title")}
                </span>
                <span className="onboarding-choice-body">
                  {t("onboarding.choose.local.body")}
                </span>
              </button>
              <button
                type="button"
                className="onboarding-choice"
                onClick={() => {
                  setErrorMsg("");
                  setPhase("cloud-input");
                }}
              >
                <span className="onboarding-choice-title">
                  {t("onboarding.choose.cloud.title")}
                </span>
                <span className="onboarding-choice-body">
                  {t("onboarding.choose.cloud.body")}
                </span>
              </button>
            </div>
          </>
        )}

        {phase === "cloud-input" && (
          <>
            <h2 className="onboarding-wizard-title">
              {t("onboarding.cloud.url.title")}
            </h2>
            <p className="onboarding-wizard-body">
              <a
                href={DEPLOY_DOCS_URL}
                target="_blank"
                rel="noreferrer"
                className="onboarding-deploy-link"
              >
                {t("onboarding.cloud.url.deploy_link")}
              </a>
            </p>
            <label
              htmlFor="onboarding-vault-url"
              className="onboarding-input-label"
            >
              {t("pair.url.label")}
            </label>
            <input
              id="onboarding-vault-url"
              type="url"
              autoFocus
              placeholder={t("onboarding.cloud.url.placeholder")}
              value={vaultUrl}
              onChange={(e) => setVaultUrl(e.target.value)}
              className="onboarding-input"
              onKeyDown={(e) => {
                if (e.key === "Enter") startCloudPair(vaultUrl);
              }}
            />
            <p className="onboarding-input-sub">{t("pair.url.sub")}</p>
            {errorMsg && <p className="onboarding-error">{errorMsg}</p>}
            <div className="onboarding-wizard-actions">
              <button
                type="button"
                className="btn"
                onClick={() => {
                  setErrorMsg("");
                  setPhase("choose");
                }}
              >
                {t("onboarding.cloud.back")}
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => startCloudPair(vaultUrl)}
              >
                {t("onboarding.cloud.continue")}
              </button>
            </div>
          </>
        )}

        {phase === "cloud-pair" && (
          <>
            <h2 className="onboarding-wizard-title">
              {t("onboarding.cloud.pair.intro")}
            </h2>
            <p className="onboarding-wizard-body">
              {t("onboarding.cloud.pair.code_label")}
            </p>
            <div className="onboarding-pair-code">{userCode}</div>
            <p className="onboarding-input-sub">
              {t("onboarding.cloud.pair.url_hint")}{" "}
              <a href={verificationUrl} target="_blank" rel="noreferrer">
                {verificationUrl}
              </a>
            </p>
            <p className="onboarding-input-sub">
              {t("pair.polling.waiting")}
            </p>
            <div className="onboarding-wizard-actions">
              <button type="button" className="btn" onClick={cancelCloudPair}>
                {t("onboarding.cloud.pair.cancel")}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
