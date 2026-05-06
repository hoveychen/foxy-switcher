import { useEffect, useRef, useState } from "react";
import { Modal } from "./Modal";
import { apiClient, getDeviceInfo, saveAgentConfig } from "../api";
import { t } from "../i18n";

// PairVaultModal walks the user through the device-flow handshake against
// a remote vault: enter URL → POST /api/pair/init on the local daemon
// (which forwards to the vault's /agent/v1/devices/pair-init) → show the
// user_code while polling /api/pair/poll → on approve, save the bearer
// token to ~/.foxy-switcher/agent-config.json via Tauri.
//
// Why through the local daemon instead of fetching the vault directly:
// Tauri's http-plugin scope only allows http://127.0.0.1:* and
// http://localhost:*. The daemon (already in scope) forwards to the
// user-supplied vault host, which has no scope restrictions on the Go
// side. This also reuses httpclient.PairInit / PairPoll, which the TUI
// `foxy-switcher pair` command already exercises.
//
// State machine:
//   form     — user is entering vault URL.
//   polling  — pair-init fired; we're showing user_code and polling.
//   approved — token persisted; nudge the user to restart the daemon.
//   error    — terminal failure; show message + Retry.
type Phase = "form" | "polling" | "approved" | "error";

export function PairVaultModal({
  open,
  onClose,
  defaultUrl,
}: {
  open: boolean;
  onClose: () => void;
  defaultUrl?: string;
}) {
  const [phase, setPhase] = useState<Phase>("form");
  const [vaultUrl, setVaultUrl] = useState(defaultUrl || "");
  const [userCode, setUserCode] = useState("");
  const [verificationUrl, setVerificationUrl] = useState("");
  const [savedPath, setSavedPath] = useState("");
  const [errorMsg, setErrorMsg] = useState("");
  const cancelRef = useRef<{ canceled: boolean }>({ canceled: false });

  // Reset whenever the modal is dismissed so re-opens land in form state.
  useEffect(() => {
    if (!open) {
      cancelRef.current.canceled = true;
      setPhase("form");
      setUserCode("");
      setVerificationUrl("");
      setSavedPath("");
      setErrorMsg("");
    } else {
      cancelRef.current = { canceled: false };
    }
  }, [open]);

  const stripTrailingSlash = (s: string) => s.replace(/\/+$/, "");

  const start = async () => {
    const url = stripTrailingSlash(vaultUrl.trim());
    if (!url) {
      setErrorMsg(t("pair.error.url_required"));
      setPhase("error");
      return;
    }
    setErrorMsg("");
    const clientNonce = `nonce-${Math.random().toString(36).slice(2)}-${Date.now()}`;
    const meta = await getDeviceInfo();
    const deviceName =
      meta?.hostname ||
      (typeof navigator !== "undefined" && navigator.platform
        ? `Foxy ${navigator.platform}`
        : "Foxy device");

    try {
      const init = await apiClient.pairInit(url, deviceName, clientNonce, meta);
      setUserCode(init.user_code);
      setVerificationUrl(init.verification_url);
      setPhase("polling");
      pollLoop(url, clientNonce, init.expires_in_ms);
    } catch (e) {
      setErrorMsg(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  };

  const pollLoop = async (
    baseUrl: string,
    clientNonce: string,
    expiresInMs: number,
  ) => {
    const deadline = Date.now() + expiresInMs;
    while (!cancelRef.current.canceled && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 2000));
      if (cancelRef.current.canceled) return;
      try {
        const out = await apiClient.pairPoll(baseUrl, clientNonce);
        if (out.status === "approved" && out.device_token && out.device_id) {
          try {
            const path = await saveAgentConfig({
              vault_url: baseUrl,
              device_id: out.device_id,
              device_token: out.device_token,
            });
            setSavedPath(path);
            setPhase("approved");
          } catch (e) {
            setErrorMsg(
              t("pair.error.save_failed") +
                ": " +
                (e instanceof Error ? e.message : String(e)),
            );
            setPhase("error");
          }
          return;
        }
        if (out.status === "denied") {
          setErrorMsg(t("pair.error.denied"));
          setPhase("error");
          return;
        }
        if (out.status === "expired") {
          setErrorMsg(t("pair.error.expired"));
          setPhase("error");
          return;
        }
      } catch {
        // Ignore transient network errors and try again — most likely
        // the vault is reloading TLS or the user's wifi blinked.
      }
    }
    if (!cancelRef.current.canceled) {
      setErrorMsg(t("pair.error.expired"));
      setPhase("error");
    }
  };

  const onCancel = () => {
    cancelRef.current.canceled = true;
    onClose();
  };

  let body: React.ReactNode = null;
  let footer: React.ReactNode = null;

  if (phase === "form" || phase === "error") {
    body = (
      <div className="settings-card" style={{ padding: 16 }}>
        <label
          style={{ display: "block", fontWeight: 500, marginBottom: 8 }}
          htmlFor="pair-vault-url"
        >
          {t("pair.url.label")}
        </label>
        <input
          id="pair-vault-url"
          type="url"
          autoFocus
          placeholder="https://vault.example.com"
          value={vaultUrl}
          onChange={(e) => setVaultUrl(e.target.value)}
          style={{
            width: "100%",
            padding: "8px 10px",
            border: "1px solid rgba(128,128,128,.45)",
            borderRadius: 4,
            background: "transparent",
            color: "inherit",
            fontSize: "1em",
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") start();
          }}
        />
        <p
          className="text-meta"
          style={{ marginTop: 8, fontSize: "0.9em", lineHeight: 1.4 }}
        >
          {t("pair.url.sub")}
        </p>
        {errorMsg && (
          <p
            style={{
              color: "#c33",
              marginTop: 8,
              fontSize: "0.9em",
            }}
          >
            {errorMsg}
          </p>
        )}
      </div>
    );
    footer = (
      <>
        <button type="button" className="btn" onClick={onCancel}>
          {t("modal.cancel")}
        </button>
        <button type="button" className="btn btn-primary" onClick={start}>
          {t("pair.start")}
        </button>
      </>
    );
  } else if (phase === "polling") {
    body = (
      <div className="settings-card" style={{ padding: 16 }}>
        <p style={{ marginTop: 0, marginBottom: 8 }}>
          {t("pair.polling.intro")}
        </p>
        <div
          style={{
            fontFamily:
              "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
            fontSize: "1.6em",
            fontWeight: 600,
            letterSpacing: "0.1em",
            textAlign: "center",
            padding: "16px 8px",
            background: "rgba(128,128,128,.15)",
            borderRadius: 6,
            margin: "12px 0",
          }}
        >
          {userCode}
        </div>
        <p style={{ fontSize: "0.9em", marginBottom: 0 }}>
          {t("pair.polling.url_hint")}{" "}
          <a href={verificationUrl} target="_blank" rel="noreferrer">
            {verificationUrl}
          </a>
        </p>
        <p
          className="text-meta"
          style={{ marginTop: 12, fontSize: "0.85em" }}
        >
          {t("pair.polling.waiting")}
        </p>
      </div>
    );
    footer = (
      <button type="button" className="btn" onClick={onCancel}>
        {t("modal.cancel")}
      </button>
    );
  } else if (phase === "approved") {
    body = (
      <div className="settings-card" style={{ padding: 16 }}>
        <p style={{ marginTop: 0 }}>{t("pair.approved.intro")}</p>
        <p style={{ fontSize: "0.85em" }} className="text-meta">
          {t("pair.approved.saved_to")}{" "}
          <code style={{ fontSize: "0.95em" }}>{savedPath}</code>
        </p>
        <p style={{ marginBottom: 0, marginTop: 12 }}>
          {t("pair.approved.next_step")}
        </p>
      </div>
    );
    footer = (
      <button type="button" className="btn btn-primary" onClick={onClose}>
        {t("modal.done")}
      </button>
    );
  }

  return (
    <Modal
      open={open}
      title={t("pair.title")}
      subtitle={t("pair.subtitle")}
      onClose={onCancel}
      footer={footer}
    >
      {body}
    </Modal>
  );
}
