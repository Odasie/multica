"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { ExternalLink, Sparkles, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { larkInstallationsOptions, larkKeys } from "@multica/core/lark";
import { api } from "@multica/core/api";
import type { LarkInstallation } from "@multica/core/types";
import { useT } from "../../i18n";

// LarkTab is the workspace settings panel for Lark Bot installations.
// Listing is member-visible; the disconnect action is admin-only (the
// backend enforces it; the UI hides the button for non-admins to match).
//
// Adding a new installation flows through the Agent detail page: the
// install URL is per-agent (each Multica Agent gets exactly one Bot —
// see the (workspace_id, agent_id) UNIQUE in lark_installation), so
// asking the user to pick an agent here would re-create that page's
// picker. The "Bind your first agent" copy in the empty state hints
// users at the right entry point.
export function LarkTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data, isLoading } = useQuery({
    ...larkInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installations = data?.installations ?? [];
  const configured = data?.configured === true;
  // install_supported tracks whether a real Lark APIClient is wired
  // server-side. When false, the OAuth callback would fail at the
  // exchange step, so we hide install entry points and surface a
  // "coming soon" notice in their place rather than send users into a
  // broken flow. Existing installations remain manageable.
  const installSupported = data?.install_supported === true;

  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteLarkInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: larkKeys.installations(wsId) });
      toast.success(t(($) => $.lark.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.lark.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  return (
    <div className="space-y-8">
      <section className="space-y-1">
        <p className="text-sm text-muted-foreground">
          {t(($) => $.lark.page_description)}
        </p>
      </section>

      {!configured ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-sm font-medium">{t(($) => $.lark.not_enabled_title)}</p>
            <p className="text-xs text-muted-foreground">
              {t(($) => $.lark.not_enabled_description_prefix)}{" "}
              <code className="rounded bg-muted px-1 py-0.5 text-[10px]">
                MULTICA_LARK_SECRET_KEY
              </code>{" "}
              {t(($) => $.lark.not_enabled_description_suffix)}{" "}
              {t(($) => $.lark.not_enabled_self_host_hint)}
            </p>
          </CardContent>
        </Card>
      ) : !installSupported && installations.length === 0 ? (
        // Real APIClient is not wired yet. We deliberately do NOT
        // direct users to the agent-detail "Bind" button because the
        // OAuth callback would fail at the exchange step. Existing
        // installations still render via the branch below; this only
        // hides the empty-state CTA when there is nothing to manage.
        <Card>
          <CardContent className="space-y-2">
            <p className="text-sm font-medium">{t(($) => $.lark.preview_title)}</p>
            <p className="text-xs text-muted-foreground">
              {t(($) => $.lark.preview_description)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <section className="space-y-3">
          <h2 className="text-sm font-semibold">{t(($) => $.lark.connected_bots)}</h2>
          {isLoading ? (
            <Card>
              <CardContent>
                <p className="text-sm text-muted-foreground">{t(($) => $.lark.loading)}</p>
              </CardContent>
            </Card>
          ) : installations.length === 0 ? (
            <Card>
              <CardContent className="space-y-2">
                <p className="text-sm font-medium">{t(($) => $.lark.empty_title)}</p>
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.lark.empty_description_prefix)}{" "}
                  <strong>{t(($) => $.lark.empty_description_cta)}</strong>{" "}
                  {t(($) => $.lark.empty_description_suffix)}
                </p>
              </CardContent>
            </Card>
          ) : (
            <Card>
              <CardContent className="divide-y">
                {installations.map((inst) => (
                  <InstallationRow
                    key={inst.id}
                    installation={inst}
                    canManage={canManage}
                    onDisconnect={() => setDisconnectTarget(inst.id)}
                  />
                ))}
              </CardContent>
            </Card>
          )}
        </section>
      )}

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.lark.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.lark.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.lark.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.lark.disconnecting)
                : t(($) => $.lark.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function InstallationRow({
  installation,
  canManage,
  onDisconnect,
}: {
  installation: LarkInstallation;
  canManage: boolean;
  onDisconnect: () => void;
}) {
  const { t } = useT("settings");
  const isActive = installation.status === "active";
  return (
    <div className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <div className="flex items-start gap-3">
        <div className="rounded-md border bg-muted/50 p-2 text-muted-foreground">
          <Sparkles className="h-4 w-4" />
        </div>
        <div className="space-y-1">
          <p className="text-sm font-medium">
            {installation.app_id}
            {!isActive && (
              <span className="ml-2 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                {t(($) => $.lark.revoked_badge)}
              </span>
            )}
          </p>
          <p className="text-xs text-muted-foreground">
            {t(($) => $.lark.bot_open_id_label)}{" "}
            <code className="text-[10px]">{installation.bot_open_id}</code>
          </p>
          <p className="text-[10px] text-muted-foreground">
            {t(($) => $.lark.installed_at_label, {
              when: new Date(installation.installed_at).toLocaleString(),
            })}
          </p>
        </div>
      </div>
      {canManage && isActive && (
        <Button variant="outline" size="sm" onClick={onDisconnect}>
          <Trash2 className="h-3 w-3" />
          {t(($) => $.lark.disconnect)}
        </Button>
      )}
    </div>
  );
}

// LarkAgentBindButton is the per-agent CTA we expose from the agent
// detail page. The Settings panel above is the management view; this
// button is the entry point.
//
// The button hides itself when the workspace does not have a real
// Lark APIClient wired (install_supported == false on the listing
// endpoint). This is the second half of the "don't expose a flow
// that's guaranteed to fail" guarantee: even if a future view
// mistakenly mounts the button before the install surface is fully
// wired, it stays invisible to users.
//
// Keeping it in the same file so a future contributor adding a Lark
// surface (e.g. an inline "you have N bots" widget on the workspace
// dashboard) finds the API client wiring next to the consumer.
export function LarkAgentBindButton({
  agentId,
  agentName,
  className,
}: {
  agentId: string;
  agentName?: string;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const [opening, setOpening] = useState(false);

  // Cheap signal that decides whether the button is reachable for this
  // workspace. The query is cached + WS-invalidated by larkKeys, so
  // mounting this button on the agent detail page does not add new
  // load — it shares the cache with the Settings tab.
  const { data: listing } = useQuery({
    ...larkInstallationsOptions(wsId),
    enabled: !!wsId,
  });
  const installSupported = listing?.install_supported === true;

  async function handleClick() {
    if (opening) return;
    setOpening(true);
    try {
      const resp = await api.getLarkInstallURL(wsId, agentId);
      if (!resp.configured || !resp.url) {
        toast.error(t(($) => $.lark.toast_oauth_not_configured));
        return;
      }
      // Open in a new tab so the in-app session keeps working; the
      // callback (LarkInstallCallback in server/internal/handler/lark.go)
      // bounces the browser back to /settings?tab=lark on success.
      window.open(resp.url, "_blank", "noopener");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.lark.toast_start_failed));
    } finally {
      setOpening(false);
    }
  }

  if (!installSupported) return null;

  return (
    <Button
      variant="outline"
      size="sm"
      onClick={handleClick}
      disabled={opening || !agentId}
      className={className}
      title={agentName ? t(($) => $.lark.bind_button_title, { agent: agentName }) : undefined}
    >
      <ExternalLink className="h-3 w-3" />
      {opening
        ? t(($) => $.lark.bind_button_opening)
        : t(($) => $.lark.bind_button)}
    </Button>
  );
}
