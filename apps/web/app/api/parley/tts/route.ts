import { NextResponse, type NextRequest } from "next/server";

// Parley TTS proxy — server-side ElevenLabs caller.
//
// The ElevenLabs key is a SERVER secret: it is read from `process.env` here
// and never shipped to the client (no NEXT_PUBLIC_/VITE_ exposure). The
// browser POSTs `{ text }`, this route adds the key and streams the audio
// back. This sits at `/api/parley/tts`, which is served by Next's
// file-system route *before* the `/api/:path*` → Go-backend rewrite
// (that rewrite is `afterFiles`, so real route handlers win — see
// apps/web/next.config.ts).
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

interface TtsRequestBody {
  text?: unknown;
  voiceId?: unknown;
}

export async function POST(req: NextRequest) {
  const apiKey = process.env.ELEVENLABS_API_KEY;
  if (!apiKey) {
    // Not an error the user needs to see — the client treats 503 as
    // "no server voice configured" and falls back to SpeechSynthesis.
    return NextResponse.json(
      { error: "tts_unconfigured" },
      { status: 503 },
    );
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

  const voiceId =
    (typeof body.voiceId === "string" && body.voiceId.trim()) ||
    process.env.ELEVENLABS_VOICE_ID ||
    DEFAULT_VOICE_ID;
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
