"use client";

import { useCallback } from "react";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import { useT } from "../i18n";

interface DesktopBridge {
  downloadURL?: (u: string) => Promise<void> | void;
}

// Detected at call time, not module load — the bridge is injected by the
// Electron preload after `window` exists, and reading it lazily lets the
// same hook work in both renderers without a build-time fork.
function hasDesktopDownloadBridge(): boolean {
  if (typeof window === "undefined") return false;
  const bridge = (window as unknown as { desktopAPI?: DesktopBridge }).desktopAPI;
  return Boolean(bridge?.downloadURL);
}

/**
 * Returns a callback that downloads an attachment by ID through a freshly
 * signed CloudFront URL. The server re-signs `download_url` on every
 * `GET /api/attachments/{id}` call, so this flow sidesteps stale signatures
 * cached in TanStack Query / inlined in markdown.
 *
 * Two execution shapes, picked at call time:
 *
 * - **Web**: open a same-origin `about:blank` tab *synchronously* inside
 *   the click handler — popup blockers (Safari especially) only consider
 *   the gesture frame, not the later async settle. The placeholder tab
 *   keeps the user activation receipt; after the fetch resolves we navigate
 *   it. We can NOT pass `"noopener"` to `window.open` because the HTML
 *   spec (`dom-open` step 17) makes that return `null`, which would leave
 *   us nothing to navigate. We disown the opener manually after the fetch.
 *
 * - **Desktop**: uses `desktopAPI.downloadURL()` which invokes Electron's
 *   native `webContents.downloadURL()`, showing a save dialog and saving
 *   the file directly. This avoids the system browser entirely and fixes
 *   the Linux/Ubuntu issue where HTML files are rendered inline instead
 *   of being downloaded.
 */
export function useDownloadAttachment(): (attachmentId: string) => Promise<void> {
  const { t } = useT("editor");
  return useCallback(
    async (attachmentId: string) => {
      const failed = () => toast.error(t(($) => $.attachment.download_failed));

      if (hasDesktopDownloadBridge()) {
        try {
          const fresh = await api.getAttachment(attachmentId);
          // Server may return a server-relative `download_url`
          // (`/api/attachments/{id}/download`) when no CloudFront
          // signer is configured — the unified download endpoint chooses
          // CloudFront/presign/proxy at request time. Electron's main-side
          // `downloadURLSafely` requires `new URL()` to parse to http/https,
          // so resolve against the configured API base before we cross the
          // bridge. Absolute URLs (legacy CloudFront / S3 presigned) pass
          // through unchanged.
          const downloadUrl = resolvePublicFileUrl(fresh.download_url);
          if (!downloadUrl) {
            failed();
            return;
          }
          const bridge = (
            window as unknown as { desktopAPI?: DesktopBridge }
          ).desktopAPI;
          await bridge!.downloadURL!(downloadUrl);
        } catch {
          failed();
        }
        return;
      }

      // Web: claim the popup permission synchronously, then hydrate the URL.
      // `window.open` here returns a WindowProxy because we deliberately
      // omit `noopener`; we revoke the back-channel ourselves once we have
      // the real URL.
      const placeholder = typeof window !== "undefined"
        ? window.open("about:blank", "_blank")
        : null;
      try {
        const fresh = await api.getAttachment(attachmentId);
        // Same relative-URL handling as desktop above. For web the
        // `apiBaseUrl` is typically empty (same-origin), so `resolvePublicFileUrl`
        // returns the path unchanged and the browser resolves it against
        // the page's origin — the existing same-origin behaviour. When a
        // non-empty base is configured (e.g. tauri/standalone shells that
        // bundle the SPA but talk to a remote API), the resolver yields an
        // absolute URL, which works for both `placeholder.location.href`
        // and the last-resort `window.location.href` redirect.
        const downloadUrl = resolvePublicFileUrl(fresh.download_url);
        if (!downloadUrl) {
          placeholder?.close();
          failed();
          return;
        }
        if (placeholder) {
          placeholder.opener = null;
          placeholder.location.href = downloadUrl;
        } else if (typeof window !== "undefined") {
          // Popup blocked outright — last-resort navigate the current tab.
          window.location.href = downloadUrl;
        }
      } catch {
        placeholder?.close();
        failed();
      }
    },
    [t],
  );
}
