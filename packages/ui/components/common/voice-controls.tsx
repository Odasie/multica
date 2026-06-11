"use client";

import { Mic } from "lucide-react";
import { useTranslation } from "react-i18next";
import { cn } from "@multica/ui/lib/utils";
import { useSpeechRecognition } from "@multica/ui/hooks/use-speech-recognition";

interface VoiceControlsProps {
  /** Receives the recognized transcript once push-to-talk settles. The caller
   *  decides what to do with it (e.g. drop it into the composer to confirm). */
  onTranscript: (transcript: string) => void;
  disabled?: boolean;
  /** BCP-47 language for recognition. Defaults to the document language. */
  lang?: string;
  className?: string;
  size?: "sm" | "default";
}

/**
 * Push-to-talk mic button (browser Web Speech API, no key). Click to start
 * listening, click again to stop; the final transcript is handed back via
 * `onTranscript`. Renders nothing where the Web Speech API is unavailable.
 *
 * First slice of Parley — voice dialog for the agent fleet. Lives in
 * `@multica/ui` so web, desktop, and future surfaces can reuse it; TTS comes
 * in a later slice.
 */
function VoiceControls({
  onTranscript,
  disabled,
  lang,
  className,
  size = "default",
}: VoiceControlsProps) {
  const { t } = useTranslation("ui");
  const { isSupported, isListening, start, stop } = useSpeechRecognition({
    lang,
    onResult: onTranscript,
  });

  // Hide entirely where the API is unavailable (Firefox, older Safari).
  if (!isSupported) return null;

  const label = isListening
    ? t(($) => $.stop_voice_input)
    : t(($) => $.start_voice_input);
  const iconSize = size === "sm" ? "h-3.5 w-3.5" : "h-4 w-4";
  const btnSize = size === "sm" ? "h-6 w-6" : "h-7 w-7";

  return (
    <button
      type="button"
      onClick={() => (isListening ? stop() : start())}
      disabled={disabled}
      aria-label={label}
      aria-pressed={isListening}
      title={label}
      className={cn(
        "inline-flex items-center justify-center rounded-full text-muted-foreground hover:bg-accent hover:text-foreground transition-colors disabled:opacity-50 disabled:pointer-events-none",
        isListening && "text-destructive hover:text-destructive",
        btnSize,
        className,
      )}
    >
      <Mic className={cn(iconSize, isListening && "animate-pulse")} />
    </button>
  );
}

export { VoiceControls, type VoiceControlsProps };
