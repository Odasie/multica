"use client";

import { useState } from "react";
import {
  ArrowLeft,
  ChevronRight,
  Cloud,
  Laptop,
  Server,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import { useQuery } from "@tanstack/react-query";
import type { AgentRuntime, ComputerDetail as ComputerDetailType, MemberWithUser } from "@multica/core/types";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import {
  activeAgentsFromError,
  useDeleteComputer,
} from "@multica/core/computers";
import { useWorkspacePaths } from "@multica/core/paths";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
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
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink, useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { formatLastSeen } from "../../runtimes/utils";

// Computer detail surface (RFC v6.1 §6.3) rendered as three tabs:
//   Overview        — hero + key facts about the daemon installation
//   Agent runtimes  — each agent_runtime row hosted by this daemon
//   Activity        — minimal lifecycle timeline (registered, last seen)
//
// The Remove flow lives here too: an admin / computer-owner triggers an
// AlertDialog; on 409 the server returns `active_agents`, which we surface
// inline rather than as a faceless error toast.
export function ComputerDetail({ computer }: { computer: ComputerDetailType }) {
  const { t } = useT("computers");
  const user = useAuthStore((s) => s.user);
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const deleteMutation = useDeleteComputer(wsId);

  const [removeOpen, setRemoveOpen] = useState(false);
  const [activeAgents, setActiveAgents] = useState<number | null>(null);

  const currentMember = user ? members.find((m) => m.user_id === user.id) : null;
  const isAdmin = currentMember
    ? currentMember.role === "owner" || currentMember.role === "admin"
    : false;
  const isComputerOwner = !!user && computer.owner_id === user.id;
  const canRemove = isAdmin || isComputerOwner;

  const ownerMember = computer.owner_id
    ? members.find((m) => m.user_id === computer.owner_id) ?? null
    : null;

  const openRemove = () => {
    setActiveAgents(null);
    setRemoveOpen(true);
  };

  const handleConfirmRemove = () => {
    deleteMutation.mutate(computer.id, {
      onSuccess: () => {
        toast.success(t(($) => $.remove.toast_removed));
        setRemoveOpen(false);
        navigation.push(paths.computers());
      },
      onError: (err) => {
        const n = activeAgentsFromError(err);
        if (n !== null) {
          setActiveAgents(n);
          return;
        }
        toast.error(
          err instanceof Error ? err.message : t(($) => $.remove.toast_failed),
        );
      },
    });
  };

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-12 shrink-0 items-center gap-2 border-b px-3">
        <Button variant="ghost" size="xs" render={<AppLink href={paths.computers()} />}>
          <ArrowLeft className="h-3 w-3" />
          {t(($) => $.detail.back)}
        </Button>
        <ChevronRight className="h-3 w-3 text-muted-foreground" />
        <span className="truncate font-mono text-xs text-foreground">
          {computer.name || computer.id.slice(0, 8)}
        </span>
        <div className="ml-auto flex items-center gap-2">
          {canRemove && (
            <Button
              variant="ghost"
              size="xs"
              className="text-muted-foreground hover:text-destructive"
              onClick={openRemove}
            >
              <Trash2 className="h-3.5 w-3.5" />
              {t(($) => $.remove.trigger)}
            </Button>
          )}
        </div>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto">
        <div className="space-y-4 p-6">
          <HeroCard computer={computer} ownerMember={ownerMember} />
          <Tabs defaultValue="overview" className="gap-4">
            <TabsList>
              <TabsTrigger value="overview">{t(($) => $.detail.tabs.overview)}</TabsTrigger>
              <TabsTrigger value="runtimes">
                {t(($) => $.detail.tabs.runtimes)}
              </TabsTrigger>
              <TabsTrigger value="activity">
                {t(($) => $.detail.tabs.activity)}
              </TabsTrigger>
            </TabsList>
            <TabsContent value="overview">
              <OverviewPane computer={computer} ownerMember={ownerMember} />
            </TabsContent>
            <TabsContent value="runtimes">
              <RuntimesPane
                runtimes={computer.runtimes ?? []}
                runtimeHref={(id) => paths.runtimeDetail(id)}
              />
            </TabsContent>
            <TabsContent value="activity">
              <ActivityPane computer={computer} />
            </TabsContent>
          </Tabs>
        </div>
      </div>

      <AlertDialog
        open={removeOpen}
        onOpenChange={(v) => {
          if (!v) setRemoveOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {activeAgents !== null
                ? t(($) => $.remove.in_use_title)
                : t(($) => $.remove.title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {activeAgents !== null
                ? t(($) => $.remove.in_use_description, { count: activeAgents })
                : t(($) => $.remove.description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.remove.cancel)}</AlertDialogCancel>
            {activeAgents === null && (
              <AlertDialogAction
                variant="destructive"
                onClick={handleConfirmRemove}
                disabled={deleteMutation.isPending}
              >
                {t(($) => $.remove.confirm)}
              </AlertDialogAction>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Hero + Overview
// ---------------------------------------------------------------------------

function HeroCard({
  computer,
  ownerMember,
}: {
  computer: ComputerDetailType;
  ownerMember: MemberWithUser | null;
}) {
  const { t } = useT("computers");
  const isOnline = computer.status === "online";
  const KindIcon = kindIcon(computer.kind);
  return (
    <div className="rounded-lg border bg-card">
      <div className="flex items-start gap-3 p-4">
        <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-md border bg-card text-muted-foreground">
          <KindIcon className="h-4 w-4" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <h2 className="truncate text-base font-semibold tracking-tight">
              {computer.name || "(unnamed)"}
            </h2>
            <Badge variant={isOnline ? "default" : "secondary"} className="text-[10px]">
              {isOnline
                ? t(($) => $.list.status.online)
                : t(($) => $.list.status.offline)}
            </Badge>
            <span className="text-xs text-muted-foreground">
              {t(($) => $.detail.last_seen, {
                when: formatLastSeen(computer.last_seen_at),
              })}
            </span>
          </div>
          <div className="mt-1 truncate text-xs text-muted-foreground">
            {kindLabel(computer.kind, t)}
            {ownerMember && (
              <>
                <span className="px-1.5 text-muted-foreground/50">·</span>
                <span className="inline-flex items-center gap-1.5 align-middle">
                  <ActorAvatar
                    actorType="member"
                    actorId={ownerMember.user_id}
                    size={14}
                  />
                  <span>{ownerMember.name}</span>
                </span>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

function OverviewPane({
  computer,
  ownerMember,
}: {
  computer: ComputerDetailType;
  ownerMember: MemberWithUser | null;
}) {
  const { t } = useT("computers");
  const facts: Array<{ label: string; value: React.ReactNode; mono?: boolean }> = [
    { label: t(($) => $.detail.fact.kind), value: kindLabel(computer.kind, t) },
    {
      label: t(($) => $.detail.fact.status),
      value: computer.status === "online"
        ? t(($) => $.list.status.online)
        : t(($) => $.list.status.offline),
    },
    {
      label: t(($) => $.detail.fact.owner),
      value: ownerMember ? ownerMember.name : t(($) => $.detail.owner_unset),
    },
    {
      label: t(($) => $.detail.fact.device),
      value: computer.device_info || "—",
      mono: true,
    },
    {
      label: t(($) => $.detail.fact.install_source),
      value: computer.install_source || "—",
      mono: true,
    },
    {
      label: t(($) => $.detail.fact.registered),
      value: formatDate(computer.created_at),
    },
    {
      label: t(($) => $.detail.fact.daemon_id),
      value: shortId(computer.id),
      mono: true,
    },
  ];

  return (
    <div className="grid grid-cols-1 gap-3 rounded-lg border bg-card p-4 sm:grid-cols-2 lg:grid-cols-3">
      {facts.map((f) => (
        <div key={f.label} className="min-w-0">
          <div className="text-[11px] uppercase tracking-wider text-muted-foreground">
            {f.label}
          </div>
          <div className={`mt-1 truncate text-sm ${f.mono ? "font-mono text-xs" : ""}`}>
            {f.value}
          </div>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Runtimes pane
// ---------------------------------------------------------------------------

function RuntimesPane({
  runtimes,
  runtimeHref,
}: {
  runtimes: AgentRuntime[];
  runtimeHref: (id: string) => string;
}) {
  const { t } = useT("computers");
  if (!runtimes.length) {
    return (
      <div className="rounded-md border border-dashed bg-card p-8 text-center text-sm text-muted-foreground">
        {t(($) => $.detail.runtimes_empty)}
      </div>
    );
  }
  return (
    <div className="overflow-hidden rounded-lg border">
      <table className="w-full text-sm">
        <thead className="bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
          <tr>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_name)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_status)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_last_seen)}</th>
            <th className="px-3 py-2" />
          </tr>
        </thead>
        <tbody>
          {runtimes.map((r) => (
            <tr key={r.id} className="border-t hover:bg-muted/30">
              <td className="px-3 py-2 font-medium">
                <AppLink href={runtimeHref(r.id)} className="hover:underline">
                  {r.name}
                </AppLink>
                <div className="text-[11px] capitalize text-muted-foreground">
                  {r.provider}
                </div>
              </td>
              <td className="px-3 py-2 text-muted-foreground">{r.status}</td>
              <td className="px-3 py-2 text-muted-foreground">
                {formatLastSeen(r.last_seen_at)}
              </td>
              <td className="px-3 py-2 text-right">
                <AppLink
                  href={runtimeHref(r.id)}
                  className="text-xs text-muted-foreground hover:text-foreground"
                >
                  {t(($) => $.detail.runtimes_link)}
                  <ChevronRight className="inline h-3 w-3 align-middle" />
                </AppLink>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Activity pane
// ---------------------------------------------------------------------------

function ActivityPane({ computer }: { computer: ComputerDetailType }) {
  const { t } = useT("computers");
  // No dedicated /api/computers/:id/activity endpoint yet — we surface the
  // two lifecycle anchors we DO know about (registered_at and last_seen_at)
  // so the tab isn't empty. When the backend grows a real activity feed
  // we'll swap this for a paginated query without changing the tab shape.
  const events = [
    {
      key: "registered",
      label: t(($) => $.detail.activity_registered),
      at: computer.created_at,
    },
    computer.last_seen_at
      ? {
          key: "last-seen",
          label: t(($) => $.detail.activity_last_seen),
          at: computer.last_seen_at,
        }
      : null,
  ].filter((e): e is { key: string; label: string; at: string } => !!e);

  if (!events.length) {
    return (
      <div className="rounded-md border border-dashed bg-card p-8 text-center text-sm text-muted-foreground">
        {t(($) => $.detail.activity_empty)}
      </div>
    );
  }

  return (
    <ol className="space-y-2">
      {events.map((e) => (
        <li
          key={e.key}
          className="flex items-center justify-between rounded-md border bg-card px-3 py-2 text-sm"
        >
          <span>{e.label}</span>
          <span className="text-xs text-muted-foreground">{formatDate(e.at)}</span>
        </li>
      ))}
    </ol>
  );
}

// ---------------------------------------------------------------------------
// Local helpers
// ---------------------------------------------------------------------------

function kindIcon(kind: ComputerDetailType["kind"]) {
  switch (kind) {
    case "desktop":
      return Laptop;
    case "cloud":
      return Cloud;
    default:
      return Server;
  }
}

function kindLabel(
  kind: ComputerDetailType["kind"],
  t: ReturnType<typeof useT<"computers">>["t"],
): string {
  switch (kind) {
    case "desktop":
      return t(($) => $.list.kind.desktop);
    case "remote":
      return t(($) => $.list.kind.remote);
    case "cloud":
      return t(($) => $.list.kind.cloud);
    default:
      return t(($) => $.list.kind.unknown);
  }
}

function formatDate(ts: string | null | undefined): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

function shortId(id: string): string {
  if (id.length <= 10) return id;
  return `${id.slice(0, 6)}··${id.slice(-2)}`;
}
