import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

import { POST } from "./route";

const DEFAULT_VOICE_ID = "21m00Tcm4TlvDq8ikWAM";

function makeRequest(
  body: unknown,
  headers: Record<string, string> = {},
): NextRequest {
  const raw = typeof body === "string" ? body : JSON.stringify(body);
  return new NextRequest("http://localhost/api/parley/tts", {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: raw,
  });
}

describe("POST /api/parley/tts", () => {
  beforeEach(() => {
    process.env.ELEVENLABS_API_KEY = "test-key";
    delete process.env.ELEVENLABS_VOICE_ID;
    delete process.env.ELEVENLABS_ALLOWED_VOICE_IDS;
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    delete process.env.ELEVENLABS_API_KEY;
    delete process.env.ELEVENLABS_VOICE_ID;
    delete process.env.ELEVENLABS_ALLOWED_VOICE_IDS;
  });

  it("returns 503 when no API key is configured", async () => {
    delete process.env.ELEVENLABS_API_KEY;
    const res = await POST(makeRequest({ text: "hi" }));
    expect(res.status).toBe(503);
  });

  it("proxies to the default voice and streams audio back (happy path)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("mp3", {
        status: 200,
        headers: { "content-type": "audio/mpeg" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(makeRequest({ text: "hello" }));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("audio/mpeg");
    const calledUrl = fetchMock.mock.calls[0]?.[0] as string;
    expect(calledUrl).toContain(`/${DEFAULT_VOICE_ID}`);
  });

  it("rejects an empty body with 400 missing_text", async () => {
    const res = await POST(makeRequest({ text: "   " }));
    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "missing_text" });
  });

  it("rejects a disallowed voiceId override with 400 (allowlist guard)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(makeRequest({ text: "hello", voiceId: "premium-voice" }));

    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "voice_not_allowed" });
    // Never reaches ElevenLabs.
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("accepts an allowlisted voiceId override", async () => {
    process.env.ELEVENLABS_ALLOWED_VOICE_IDS = "brand-voice, other-voice";
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("mp3", {
        status: 200,
        headers: { "content-type": "audio/mpeg" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(makeRequest({ text: "hello", voiceId: "brand-voice" }));

    expect(res.status).toBe(200);
    const calledUrl = fetchMock.mock.calls[0]?.[0] as string;
    expect(calledUrl).toContain("/brand-voice");
  });

  it("rejects an oversized body before parsing with 413 (pre-parse guard)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const res = await POST(
      makeRequest({ text: "hi" }, { "content-length": String(10 * 1024 * 1024) }),
    );

    expect(res.status).toBe(413);
    expect(await res.json()).toEqual({ error: "body_too_large" });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
