import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

import { POST } from "./route";

const TTS_URL = "http://localhost/api/parley/tts";
const ELEVENLABS_HOST = "api.elevenlabs.io";

function makeRequest(opts: {
  cookie?: string;
  body?: string;
  contentLength?: string;
}): NextRequest {
  const body = opts.body ?? JSON.stringify({ text: "hello" });
  const headers = new Headers({ "content-type": "application/json" });
  if (opts.cookie) headers.set("cookie", opts.cookie);
  headers.set(
    "content-length",
    opts.contentLength ?? String(Buffer.byteLength(body)),
  );
  return new NextRequest(TTS_URL, { method: "POST", headers, body });
}

/**
 * Fetch double: GET /api/me is the Go session check, POST to ElevenLabs is the
 * paid upstream. `authed` toggles whether /api/me returns 200; `userId` is the
 * id /api/me reports — vary it per test so the module-level rate-limit bucket
 * (keyed on user id) does not leak state across tests.
 */
function installFetchMock({
  authed,
  userId = "user-1",
}: {
  authed: boolean;
  userId?: string;
}) {
  const mock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.includes("/api/me")) {
      return authed
        ? new Response(JSON.stringify({ id: userId }), { status: 200 })
        : new Response('{"error":"missing authorization"}', { status: 401 });
    }
    if (url.includes(ELEVENLABS_HOST)) {
      return new Response("FAKE_MP3_BYTES", {
        status: 200,
        headers: { "content-type": "audio/mpeg" },
      });
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

function elevenLabsCalls(mock: ReturnType<typeof vi.fn>) {
  return mock.mock.calls.filter(([input]) =>
    String(input).includes(ELEVENLABS_HOST),
  );
}

describe("POST /api/parley/tts", () => {
  beforeEach(() => {
    process.env.ELEVENLABS_API_KEY = "test-key";
    process.env.REMOTE_API_URL = "http://localhost:8080";
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    delete process.env.ELEVENLABS_API_KEY;
    delete process.env.REMOTE_API_URL;
    delete process.env.ELEVENLABS_ALLOWED_VOICE_IDS;
  });

  it("streams audio for an authenticated request", async () => {
    const mock = installFetchMock({ authed: true, userId: "user-happy" });
    const res = await POST(makeRequest({ cookie: "multica_auth=valid" }));

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("audio/mpeg");
    expect(elevenLabsCalls(mock)).toHaveLength(1);
  });

  it("rejects a disallowed voiceId override with 400 and never calls ElevenLabs", async () => {
    const mock = installFetchMock({ authed: true, userId: "user-voice-deny" });
    const res = await POST(
      makeRequest({
        cookie: "multica_auth=valid",
        body: JSON.stringify({ text: "hello", voiceId: "premium-voice" }),
      }),
    );

    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "voice_not_allowed" });
    expect(elevenLabsCalls(mock)).toHaveLength(0);
  });

  it("accepts an allowlisted voiceId override via ELEVENLABS_ALLOWED_VOICE_IDS", async () => {
    process.env.ELEVENLABS_ALLOWED_VOICE_IDS = "brand-voice, other-voice";
    const mock = installFetchMock({ authed: true, userId: "user-voice-allow" });
    const res = await POST(
      makeRequest({
        cookie: "multica_auth=valid",
        body: JSON.stringify({ text: "hello", voiceId: "brand-voice" }),
      }),
    );

    expect(res.status).toBe(200);
    const calls = elevenLabsCalls(mock);
    expect(calls).toHaveLength(1);
    expect(String(calls[0]?.[0])).toContain("/brand-voice");
  });

  it("returns 401 with no session cookie and never calls ElevenLabs", async () => {
    const mock = installFetchMock({ authed: false });
    const res = await POST(makeRequest({}));

    expect(res.status).toBe(401);
    expect(elevenLabsCalls(mock)).toHaveLength(0);
  });

  it("returns 401 when Go rejects the session and never calls ElevenLabs", async () => {
    const mock = installFetchMock({ authed: false });
    const res = await POST(makeRequest({ cookie: "multica_auth=stale" }));

    expect(res.status).toBe(401);
    expect(elevenLabsCalls(mock)).toHaveLength(0);
  });

  it("rejects an oversized body on Content-Length before parsing", async () => {
    const mock = installFetchMock({ authed: true });
    const res = await POST(
      makeRequest({
        cookie: "multica_auth=valid",
        // 5000-char cap → MAX_BODY_BYTES ≈ 21024; go well past it.
        contentLength: "999999",
      }),
    );

    expect(res.status).toBe(413);
    // No upstream call (and no session round-trip either — guard is first).
    expect(mock).not.toHaveBeenCalled();
  });

  it("throttles a session with 429 once the token bucket is empty", async () => {
    const mock = installFetchMock({ authed: true, userId: "user-rl" });
    // Capacity is 20; the 21st request within the window is throttled.
    let lastStatus = 0;
    for (let i = 0; i < 21; i++) {
      const res = await POST(makeRequest({ cookie: "multica_auth=valid" }));
      lastStatus = res.status;
    }
    expect(lastStatus).toBe(429);
    // Exactly 20 paid calls fired; the throttled one did not.
    expect(elevenLabsCalls(mock)).toHaveLength(20);
  });
});
