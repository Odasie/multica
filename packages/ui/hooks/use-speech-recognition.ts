import { useCallback, useEffect, useRef, useState } from "react";

// Minimal Web Speech API typings. lib.dom does not ship `SpeechRecognition`
// reliably across the TS versions we target, and `webkitSpeechRecognition`
// is never typed there — so we declare the slice we use locally to keep the
// package self-contained and free of an ambient global.
interface SpeechAlternative {
  transcript: string;
}
interface SpeechResult {
  readonly isFinal: boolean;
  readonly length: number;
  [index: number]: SpeechAlternative;
}
interface SpeechResultList {
  readonly length: number;
  [index: number]: SpeechResult;
}
interface SpeechResultEvent {
  readonly resultIndex: number;
  readonly results: SpeechResultList;
}
interface SpeechErrorEvent {
  readonly error: string;
}
interface SpeechRecognitionInstance {
  lang: string;
  continuous: boolean;
  interimResults: boolean;
  start: () => void;
  stop: () => void;
  abort: () => void;
  onresult: ((event: SpeechResultEvent) => void) | null;
  onerror: ((event: SpeechErrorEvent) => void) | null;
  onend: (() => void) | null;
}
type SpeechRecognitionCtor = new () => SpeechRecognitionInstance;

function getSpeechRecognitionCtor(): SpeechRecognitionCtor | null {
  if (typeof window === "undefined") return null;
  const w = window as unknown as {
    SpeechRecognition?: SpeechRecognitionCtor;
    webkitSpeechRecognition?: SpeechRecognitionCtor;
  };
  return w.SpeechRecognition ?? w.webkitSpeechRecognition ?? null;
}

interface UseSpeechRecognitionOptions {
  /** BCP-47 language tag for recognition (e.g. "en-US"). Defaults to the
   *  document language, then "en-US". */
  lang?: string;
  /** Called with the final transcript once recognition settles. */
  onResult: (transcript: string) => void;
  /** Called with the Web Speech error code (e.g. "not-allowed", "no-speech"). */
  onError?: (error: string) => void;
}

interface UseSpeechRecognitionReturn {
  /** True once the Web Speech API is confirmed available (after mount, so it
   *  agrees with the server render and avoids a hydration mismatch). */
  isSupported: boolean;
  isListening: boolean;
  start: () => void;
  stop: () => void;
}

/**
 * Push-to-talk wrapper around the browser Web Speech API. Lazily creates a
 * single `SpeechRecognition` instance and exposes start/stop plus support and
 * listening flags. No keys, no network of our own — recognition runs in the
 * browser. Unsupported browsers (Firefox, older Safari) report
 * `isSupported: false` so callers can hide the affordance.
 */
export function useSpeechRecognition({
  lang,
  onResult,
  onError,
}: UseSpeechRecognitionOptions): UseSpeechRecognitionReturn {
  const [isSupported, setIsSupported] = useState(false);
  const [isListening, setIsListening] = useState(false);
  const recognitionRef = useRef<SpeechRecognitionInstance | null>(null);
  // Keep latest callbacks in refs so the single recognition instance always
  // invokes the current handlers without being recreated on every render.
  const onResultRef = useRef(onResult);
  const onErrorRef = useRef(onError);
  onResultRef.current = onResult;
  onErrorRef.current = onError;

  // Feature-detect after mount: on the server and the first client render this
  // stays false (renders nothing), then flips true once we know `window` has
  // the API — so SSR and hydration agree.
  useEffect(() => {
    setIsSupported(getSpeechRecognitionCtor() !== null);
  }, []);

  // Abort any in-flight recognition on unmount.
  useEffect(() => () => recognitionRef.current?.abort(), []);

  const start = useCallback(() => {
    const Ctor = getSpeechRecognitionCtor();
    if (!Ctor) return;
    if (!recognitionRef.current) {
      const rec = new Ctor();
      rec.continuous = false;
      rec.interimResults = false;
      rec.onresult = (event) => {
        let transcript = "";
        for (let i = event.resultIndex; i < event.results.length; i++) {
          const result = event.results[i];
          if (result?.isFinal) transcript += result[0]?.transcript ?? "";
        }
        const text = transcript.trim();
        if (text) onResultRef.current(text);
      };
      rec.onerror = (event) => {
        setIsListening(false);
        onErrorRef.current?.(event.error);
      };
      rec.onend = () => setIsListening(false);
      recognitionRef.current = rec;
    }
    const rec = recognitionRef.current;
    rec.lang =
      lang ||
      (typeof document !== "undefined" ? document.documentElement.lang : "") ||
      "en-US";
    try {
      rec.start();
      setIsListening(true);
    } catch {
      // start() throws if recognition is already running — keep listening.
    }
  }, [lang]);

  const stop = useCallback(() => {
    recognitionRef.current?.stop();
    setIsListening(false);
  }, []);

  return { isSupported, isListening, start, stop };
}
