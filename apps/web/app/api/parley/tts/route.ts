import { NextResponse, type NextRequest } from "next/server";

import { resolveRemoteApiUrl } from "@/config/runtime-urls";

// Parley TTS proxy — server-side ElevenLabs caller.
//
// The ElevenLabs key is a SERVER secret: it is read from `process.env` here
// and never shipped to the client (no NEXT_PUBLIC_/VITE_ exposure). The
// browser POSTs `{ text }`, this route adds the key and streams the audio
// back. This sits at `/api/parley/tts`, which is served by Next's
// file-system route *before* the `/api/:path*` → Go-backend rewrite
// (that rewrite is `afterFiles`, so real route handlers win — see
// apps/web/next.config.ts). Because this handler wins over the rewrite the Go
// auth layer never sees the request, so this route must gate itself — it
// forward-authenticates to Go below before doing any paid upstream work.
//
// When the key is absent (local dev, no-key deploys) the route returns 503
// so the client falls back to the browser SpeechSynthesis voice. That makes
// the feature degrade gracefully instead of erroring.

export const runtime = "nodejs";
// Reads request body + a server secret per call — never statically rendered.
export const dynamic = "force-dynamic";

const ELEVENLABS_BASE = "https://api.elevenlabs.io/v1/text-to-speech";
// Rachel — ElevenLabs' default public voice. Overridable per-request or via
// ELEVENLABS_VOICE_ID so the deploy can pick a brand voice without a code change.
const DEFAULT_VOICE_ID = "21m00Tcm4TlvDq8ikWAM";
const DEFAULT_MODEL_ID = "eleven_multilingual_v2";
// Guardrail: agent replies are short, but cap the proxied text so a runaway
// message can't run up the ElevenLabs bill or hang the request.
const MAX_TEXT_LENGTH = 5000;
// Pre-parse Content-Length cap. A 5000-char string is at most ~4 bytes/char in
// UTF-8; add headroom for the JSON envelope and an optional `voiceId`. We reject
// on Content-Length BEFORE `req.json()` so an oversized payload is never
// buffered or parsed.
const MAX_BODY_BYTES = MAX_TEXT_LENGTH * 4 + 1024;

// Go's session-validation endpoint: returns 200 for an authenticated session
// and 401 otherwise, reading the `multica_auth` HttpOnly cookie. Forward-auth
// keeps Go the single source of truth for sessions — Next never needs the
// session secret or DB access.
const SESSION_VALIDATE_PATH = "/api/me";
// Validation is a localhost round-trip; keep it short and fail closed.
const SESSION_VALIDATE_TIMEOUT_MS = 3000;

// Per-session in-memory token bucket. Single-VPS self-host makes in-memory
// adequate; a horizontally-scaled deploy would need a shared store (e.g. Redis)
// so the limit holds across instances.
const RATE_LIMIT_CAPACITY = 20; // burst size = requests per window
const RATE_LIMIT_WINDOW_MS = 60_000; // bucket refills fully once per minute
const RATE_LIMIT_REFILL_PER_MS = RATE_LIMIT_CAPACITY / RATE_LIMIT_WINDOW_MS;

interface Bucket {
  tokens: number;
  lastRefill: number;
}
const buckets = new Map<string, Bucket>();

// Returns false when the caller has exhausted their tokens (→ 429).
function allowRequest(sessionKey: string): boolean {
  const now = Date.now();
  const bucket = buckets.get(sessionKey) ?? {
    tokens: RATE_LIMIT_CAPACITY,
    lastRefill: now,
  };
  bucket.tokens = Math.min(
    RATE_LIMIT_CAPACITY,
    bucket.tokens + (now - bucket.lastRefill) * RATE_LIMIT_REFILL_PER_MS,
  );
  bucket.lastRefill = now;
  buckets.set(sessionKey, bucket);
  if (bucket.tokens < 1) return false;
  bucket.tokens -= 1;
  return true;
}

// Forward-authenticate to Go. Returns a stable per-session key on success, or
// null when the session is invalid/unreachable (fail closed → 401).
async function validateSession(cookie: string): Promise<string | null> {
  const base = resolveRemoteApiUrl(process.env);
  const controller = new AbortController();
  const timer = setTimeout(
    () => controller.abort(),
    SESSION_VALIDATE_TIMEOUT_MS,
  );
  try {
    const res = await fetch(`${base}${SESSION_VALIDATE_PATH}`, {
      method: "GET",
      headers: { cookie },
      signal: controller.signal,
    });
    if (!res.ok) return null;
    // Prefer the user id as the rate-limit key; fall back to the cookie so a
    // shape change in /api/me can never silently disable the limit.
    try {
      const me = (await res.json()) as { id?: unknown };
      if (typeof me.id === "string" && me.id) return me.id;
    } catch {
      // 200 with an unparseable body — still authenticated; key on the cookie.
    }
    return cookie;
  } catch {
    // Timeout or network error reaching Go — never fall through to ElevenLabs.
    return null;
  } finally {
    clearTimeout(timer);
  }
}

interface TtsRequestBody {
  text?: unknown;
  voiceId?: unknown;
}

// Voice IDs the proxy accepts on a per-request override. Anything outside this
// set is rejected (400) so a caller can't select an arbitrary — possibly
// premium — ElevenLabs voice by smuggling a voiceId in the body. The set is the
// built-in default, the deploy's configured voice, and any extra IDs the deploy
// opts into via ELEVENLABS_ALLOWED_VOICE_IDS (comma-separated).
function allowedVoiceIds(): Set<string> {
  const ids = new Set<string>([DEFAULT_VOICE_ID]);
  const configured = process.env.ELEVENLABS_VOICE_ID?.trim();
  if (configured) ids.add(configured);
  for (const raw of (process.env.ELEVENLABS_ALLOWED_VOICE_IDS ?? "").split(",")) {
    const id = raw.trim();
    if (id) ids.add(id);
  }
  return ids;
}

export async function POST(req: NextRequest) {
  // Reject oversized payloads before parsing — cheap DoS / bill guard.
  const contentLength = Number(req.headers.get("content-length"));
  if (Number.isFinite(contentLength) && contentLength > MAX_BODY_BYTES) {
    return NextResponse.json({ error: "text_too_long" }, { status: 413 });
  }

  const apiKey = process.env.ELEVENLABS_API_KEY;
  if (!apiKey) {
    // Not an error the user needs to see — the client treats 503 as
    // "no server voice configured" and falls back to SpeechSynthesis.
    return NextResponse.json(
      { error: "tts_unconfigured" },
      { status: 503 },
    );
  }

  // Gate: forward-authenticate to Go before any paid upstream work.
  const cookie = req.headers.get("cookie") ?? "";
  const sessionKey = cookie ? await validateSession(cookie) : null;
  if (!sessionKey) {
    return NextResponse.json({ error: "unauthorized" }, { status: 401 });
  }

  // Per-session rate limit — throttle before firing the ElevenLabs request.
  if (!allowRequest(sessionKey)) {
    return NextResponse.json({ error: "rate_limited" }, { status: 429 });
  }

  let body: TtsRequestBody;
  try {
    body = (await req.json()) as TtsRequestBody;
  } catch {
    return NextResponse.json({ error: "invalid_json" }, { status: 400 });
  }

  const text = typeof body.text === "string" ? body.text.trim() : "";
  if (!text) {
    return NextResponse.json({ error: "missing_text" }, { status: 400 });
  }
  if (text.length > MAX_TEXT_LENGTH) {
    return NextResponse.json({ error: "text_too_long" }, { status: 413 });
  }

  const requestedVoiceId =
    typeof body.voiceId === "string" ? body.voiceId.trim() : "";
  if (requestedVoiceId && !allowedVoiceIds().has(requestedVoiceId)) {
    return NextResponse.json({ error: "voice_not_allowed" }, { status: 400 });
  }
  const voiceId =
    requestedVoiceId || process.env.ELEVENLABS_VOICE_ID || DEFAULT_VOICE_ID;
  const modelId = process.env.ELEVENLABS_MODEL_ID || DEFAULT_MODEL_ID;

  let upstream: Response;
  try {
    upstream = await fetch(`${ELEVENLABS_BASE}/${encodeURIComponent(voiceId)}`, {
      method: "POST",
      headers: {
        "xi-api-key": apiKey,
        accept: "audio/mpeg",
        "content-type": "application/json",
      },
      body: JSON.stringify({ text, model_id: modelId }),
    });
  } catch {
    // Network-level failure reaching ElevenLabs — let the client fall back.
    return NextResponse.json({ error: "tts_upstream_unreachable" }, { status: 502 });
  }

  if (!upstream.ok || !upstream.body) {
    // Surface the upstream status but NEVER the body (it can echo the key in
    // some error shapes). The client falls back to the browser voice on any
    // non-OK status.
    return NextResponse.json(
      { error: "tts_upstream_error", status: upstream.status },
      { status: 502 },
    );
  }

  // Stream the MP3 straight through — no buffering of the whole clip.
  return new NextResponse(upstream.body, {
    status: 200,
    headers: {
      "content-type": upstream.headers.get("content-type") ?? "audio/mpeg",
      "cache-control": "no-store",
    },
  });
}
