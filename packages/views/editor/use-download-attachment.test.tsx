import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";

// Hoisted mock for the API singleton: vi.mock factories cannot reference
// outside-of-scope vars, but vi.hoisted runs before the import graph.
const getAttachmentMock = vi.hoisted(() => vi.fn());
const getBaseUrlMock = vi.hoisted(() => vi.fn(() => ""));

vi.mock("@multica/core/api", () => ({
  api: { getAttachment: getAttachmentMock, getBaseUrl: getBaseUrlMock },
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

vi.mock("../i18n", () => ({
  useT: () => ({ t: (sel: (s: { attachment: { download_failed: string } }) => string) => sel({ attachment: { download_failed: "Couldn't fetch a download link. Try again in a moment." } }) }),
}));

import { useDownloadAttachment } from "./use-download-attachment";
import { toast } from "sonner";

const SIGNED_URL =
  "https://static.example.test/file.md?Policy=p&Signature=s&Key-Pair-Id=k";

beforeEach(() => {
  vi.clearAllMocks();
  // Default: web with same-origin API (empty base). Each test that needs
  // a non-empty base (desktop standalone, server-relative download URL)
  // overrides via getBaseUrlMock.mockReturnValue(...).
  getBaseUrlMock.mockReturnValue("");
});

afterEach(() => {
  vi.restoreAllMocks();
  // Scrub the desktop bridge between tests so suites don't leak state.
  delete (window as unknown as { desktopAPI?: unknown }).desktopAPI;
});

describe("useDownloadAttachment (web)", () => {
  it("opens a placeholder tab synchronously, then navigates it to the freshly signed URL", async () => {
    getAttachmentMock.mockResolvedValueOnce({
      id: "att-1",
      url: "https://static.example.test/file.md",
      download_url: SIGNED_URL,
      filename: "file.md",
    });

    const placeholder = { opener: window, location: { href: "about:blank" }, close: vi.fn() };
    const openSpy = vi.spyOn(window, "open").mockReturnValue(placeholder as unknown as Window);

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    // Placeholder MUST be opened synchronously during the click — otherwise
    // popup blockers won't honour the gesture.
    expect(openSpy).toHaveBeenCalledWith("about:blank", "_blank");
    expect(getAttachmentMock).toHaveBeenCalledWith("att-1");
    // Disown the opener and redirect to the signed URL.
    expect(placeholder.opener).toBeNull();
    expect(placeholder.location.href).toBe(SIGNED_URL);
  });

  it("closes the placeholder and shows a toast when the fetch fails", async () => {
    getAttachmentMock.mockRejectedValueOnce(new Error("boom"));
    const placeholder = { opener: window, location: { href: "about:blank" }, close: vi.fn() };
    vi.spyOn(window, "open").mockReturnValue(placeholder as unknown as Window);

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    expect(placeholder.close).toHaveBeenCalled();
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
  });
});

describe("useDownloadAttachment (desktop)", () => {
  it("skips the placeholder tab and hands the signed URL to the desktop download bridge", async () => {
    const downloadURL = vi.fn();
    (window as unknown as { desktopAPI: { downloadURL: typeof downloadURL } }).desktopAPI = {
      downloadURL,
    };
    getAttachmentMock.mockResolvedValueOnce({
      id: "att-1",
      url: "https://static.example.test/file.md",
      download_url: SIGNED_URL,
      filename: "file.md",
    });
    const openSpy = vi.spyOn(window, "open");

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    // No placeholder — Electron's setWindowOpenHandler would reject
    // about:blank, so we go straight to the platform's IPC bridge.
    expect(openSpy).not.toHaveBeenCalled();
    expect(downloadURL).toHaveBeenCalledWith(SIGNED_URL);
  });

  it("shows a toast when the API rejects on desktop", async () => {
    const downloadURL = vi.fn();
    (window as unknown as { desktopAPI: { downloadURL: typeof downloadURL } }).desktopAPI = {
      downloadURL,
    };
    getAttachmentMock.mockRejectedValueOnce(new Error("network failure"));

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    expect(downloadURL).not.toHaveBeenCalled();
    await waitFor(() => expect(toast.error).toHaveBeenCalled());
  });

  // MUL-2976: when the backend has no CloudFront signer, `getAttachment`
  // returns a server-relative `download_url` like `/api/attachments/.../download`.
  // The Electron main-process `downloadURLSafely` requires a parsable
  // http(s) URL or it drops the request — so the renderer must resolve
  // the path against the configured API base before crossing the bridge.
  it("resolves a server-relative download_url against the API base before handing it to the desktop bridge", async () => {
    const downloadURL = vi.fn();
    (window as unknown as { desktopAPI: { downloadURL: typeof downloadURL } }).desktopAPI = {
      downloadURL,
    };
    getBaseUrlMock.mockReturnValue("https://api.example.test");
    getAttachmentMock.mockResolvedValueOnce({
      id: "att-1",
      url: "https://static.example.test/file.md",
      download_url: "/api/attachments/att-1/download",
      filename: "file.md",
    });

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    expect(downloadURL).toHaveBeenCalledWith(
      "https://api.example.test/api/attachments/att-1/download",
    );
  });

  it("trims a trailing slash on the API base when resolving a relative download_url", async () => {
    const downloadURL = vi.fn();
    (window as unknown as { desktopAPI: { downloadURL: typeof downloadURL } }).desktopAPI = {
      downloadURL,
    };
    getBaseUrlMock.mockReturnValue("https://api.example.test/");
    getAttachmentMock.mockResolvedValueOnce({
      id: "att-1",
      url: "/api/attachments/att-1/content",
      download_url: "/api/attachments/att-1/download",
      filename: "file.md",
    });

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    expect(downloadURL).toHaveBeenCalledWith(
      "https://api.example.test/api/attachments/att-1/download",
    );
  });

  it("passes an already-absolute download_url through unchanged when the bridge is present", async () => {
    const downloadURL = vi.fn();
    (window as unknown as { desktopAPI: { downloadURL: typeof downloadURL } }).desktopAPI = {
      downloadURL,
    };
    // Even with a non-empty base configured, a CloudFront signed URL
    // must not be re-prefixed.
    getBaseUrlMock.mockReturnValue("https://api.example.test");
    getAttachmentMock.mockResolvedValueOnce({
      id: "att-1",
      url: "https://cdn.example.test/att-1.bin",
      download_url: SIGNED_URL,
      filename: "file.md",
    });

    const { result } = renderHook(() => useDownloadAttachment());

    await act(async () => {
      await result.current("att-1");
    });

    expect(downloadURL).toHaveBeenCalledWith(SIGNED_URL);
  });
});
