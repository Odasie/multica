"use client";

import { Volume2, VolumeX } from "lucide-react";
import { useTranslation } from "react-i18next";
import { cn } from "@multica/ui/lib/utils";

interface AutoSpeakToggleProps {
  /** Whether auto-speak is currently on. */
  enabled: boolean;
  /** Flip the preference. */
  onToggle: () => void;
  disabled?: boolean;
  className?: string;
  size?: "sm" | "default";
}

/**
 * Toggle for Parley auto-speak: when on, agent replies are read aloud (via the
 * server-side ElevenLabs proxy, browser SpeechSynthesis fallback — see
 * `useTextToSpeech`). This button only owns the on/off affordance; the actual
 * speaking is wired where the reply stream lives (the chat window). Lives in
 * `@multica/ui` alongside `VoiceControls` so every chat surface reuses it.
 *
 * Renders nothing where the browser can't speak at all (no Audio and no
 * SpeechSynthesis), mirroring VoiceControls' hide-when-unsupported behaviour.
 */
function AutoSpeakToggle({
  enabled,
  onToggle,
  disabled,
  className,
  size = "default",
}: AutoSpeakToggleProps) {
  const { t } = useTranslation("ui");

  const supported =
    typeof window !== "undefined" &&
    ("speechSynthesis" in window || typeof Audio !== "undefined");
  if (!supported) return null;

  const label = enabled
    ? t(($) => $.stop_auto_speak)
    : t(($) => $.start_auto_speak);
  const iconSize = size === "sm" ? "h-3.5 w-3.5" : "h-4 w-4";
  const btnSize = size === "sm" ? "h-6 w-6" : "h-7 w-7";
  const Icon = enabled ? Volume2 : VolumeX;

  return (
    <button
      type="button"
      onClick={onToggle}
      disabled={disabled}
      aria-label={label}
      aria-pressed={enabled}
      title={label}
      className={cn(
        "inline-flex items-center justify-center rounded-full text-muted-foreground hover:bg-accent hover:text-foreground transition-colors disabled:opacity-50 disabled:pointer-events-none",
        enabled && "text-brand hover:text-brand",
        btnSize,
        className,
      )}
    >
      <Icon className={iconSize} />
    </button>
  );
}

export { AutoSpeakToggle, type AutoSpeakToggleProps };
