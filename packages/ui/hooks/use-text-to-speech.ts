import { useCallback, useEffect, useRef, useState } from "react";

// Default endpoint of the server-side Parley TTS proxy (apps/web). The proxy
// holds the ElevenLabs key; this hook only ever sends `{ text }`.
const DEFAULT_TTS_ENDPOINT = "/api/parley/tts";

interface UseTextToSpeechOptions {
  /** Override the proxy endpoint (tests / alternate mounts). */
  endpoint?: string;
  /** BCP-47 language for the SpeechSynthesis fallback. Defaults to the
   *  document language, then "en-US". */
  lang?: string;
}

interface UseTextToSpeechReturn {
  /** True once we know the browser can play audio at all (Audio element or
   *  SpeechSynthesis). False on the server and the first client render so SSR
   *  and hydration agree, then flips after mount. */
  isSupported: boolean;
  isSpeaking: boolean;
  /** Speak `text`: try the ElevenLabs proxy first, fall back to the browser
   *  SpeechSynthesis voice when the proxy is unconfigured/unreachable. */
  speak: (text: string) => Promise<void>;
  /** Stop any in-flight playback (proxy audio or browser speech). */
  cancel: () => void;
}

function resolveLang(lang?: string): string {
  return (
    lang ||
    (typeof document !== "undefined" ? document.documentElement.lang : "") ||
    "en-US"
  );
}

/**
 * Reads text aloud for Parley's auto-speak. Prefers the server-side
 * ElevenLabs proxy (natural voice, key stays server-side); on any failure —
 * proxy returns 503 because no key is configured, a network error, or an
 * unsupported response — it falls back to the browser's built-in
 * SpeechSynthesis so the feature still works with no key.
 *
 * A single in-flight playback at a time: each `speak()` (and `cancel()`)
 * supersedes the previous one, so a burst of replies doesn't overlap.
 */
export function useTextToSpeech({
  endpoint = DEFAULT_TTS_ENDPOINT,
  lang,
}: UseTextToSpeechOptions = {}): UseTextToSpeechReturn {
  const [isSupported, setIsSupported] = useState(false);
  const [isSpeaking, setIsSpeaking] = useState(false);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  // Monotonic token: each speak() bumps it so a slow proxy response from a
  // superseded call can't start playing over a newer one.
  const playTokenRef = useRef(0);

  useEffect(() => {
    setIsSupported(
      typeof window !== "undefined" &&
        ("speechSynthesis" in window || typeof Audio !== "undefined"),
    );
  }, []);

  const stopPlayback = useCallback(() => {
    if (audioRef.current) {
      audioRef.current.pause();
      const src = audioRef.current.src;
      audioRef.current.src = "";
      if (src.startsWith("blob:")) URL.revokeObjectURL(src);
      audioRef.current = null;
    }
    if (typeof window !== "undefined" && "speechSynthesis" in window) {
      window.speechSynthesis.cancel();
    }
  }, []);

  const cancel = useCallback(() => {
    playTokenRef.current += 1;
    stopPlayback();
    setIsSpeaking(false);
  }, [stopPlayback]);

  const speakWithBrowser = useCallback(
    (text: string, token: number) => {
      if (typeof window === "undefined" || !("speechSynthesis" in window)) {
        setIsSpeaking(false);
        return;
      }
      const utterance = new SpeechSynthesisUtterance(text);
      utterance.lang = resolveLang(lang);
      utterance.onend = () => {
        if (playTokenRef.current === token) setIsSpeaking(false);
      };
      utterance.onerror = () => {
        if (playTokenRef.current === token) setIsSpeaking(false);
      };
      window.speechSynthesis.speak(utterance);
    },
    [lang],
  );

  const speak = useCallback(
    async (text: string) => {
      const trimmed = text.trim();
      if (!trimmed) return;
      // Supersede any current/previous playback.
      cancel();
      const token = playTokenRef.current;
      setIsSpeaking(true);

      let blob: Blob | null = null;
      try {
        const res = await fetch(endpoint, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ text: trimmed }),
        });
        // 503 (no key) / 502 (upstream) / any non-OK → browser fallback.
        if (res.ok && res.headers.get("content-type")?.startsWith("audio/")) {
          blob = await res.blob();
        }
      } catch {
        // Network error reaching the proxy — fall through to the browser voice.
      }

      // A newer speak()/cancel() ran while we awaited — drop this result.
      if (playTokenRef.current !== token) return;

      if (!blob) {
        speakWithBrowser(trimmed, token);
        return;
      }

      const url = URL.createObjectURL(blob);
      const audio = new Audio(url);
      audioRef.current = audio;
      const done = () => {
        if (playTokenRef.current === token) setIsSpeaking(false);
        URL.revokeObjectURL(url);
      };
      audio.onended = done;
      audio.onerror = () => {
        // Playback failed after a good fetch — last-ditch browser fallback.
        URL.revokeObjectURL(url);
        if (playTokenRef.current === token) speakWithBrowser(trimmed, token);
      };
      try {
        await audio.play();
      } catch {
        URL.revokeObjectURL(url);
        if (playTokenRef.current === token) speakWithBrowser(trimmed, token);
      }
    },
    [cancel, endpoint, speakWithBrowser],
  );

  // Stop audio if the consumer unmounts mid-playback.
  useEffect(() => () => stopPlayback(), [stopPlayback]);

  return { isSupported, isSpeaking, speak, cancel };
}
