import { useEffect } from "react";
import { Icon } from "../components/Icon";
import { ICON_X } from "../components/icons";
import { t } from "../i18n";
import { FoxyIntroPlayer } from "./FoxyIntroPlayer";

export function OnboardingOverlay({ onDismiss }: { onDismiss: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onDismiss();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onDismiss]);

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
          onClick={onDismiss}
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
          onEnded={onDismiss}
        />
      </div>
    </div>
  );
}
