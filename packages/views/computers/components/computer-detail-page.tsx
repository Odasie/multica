"use client";

import { Monitor } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { computerDetailOptions } from "@multica/core/computers";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { ComputerDetail } from "./computer-detail";
import { useT } from "../../i18n";

/**
 * Routed entry for `/{slug}/computers/{id}`. Fetches the computer
 * aggregate (rollup + nested runtimes[]) via computerDetailOptions, which
 * the install step also primed if the user came through Add Computer. The
 * detail surface itself lives in `computer-detail.tsx` so the route layer
 * is just loading state + 404.
 */
export function ComputerDetailPage({ computerId }: { computerId: string }) {
  const { t } = useT("computers");
  const wsId = useWorkspaceId();
  const { data: computer, isLoading, isError } = useQuery({
    ...computerDetailOptions(wsId ?? "", computerId),
    enabled: !!wsId,
  });

  if (isLoading) {
    return (
      <div className="flex h-full flex-col p-6">
        <Skeleton className="h-12 w-1/2" />
        <div className="mt-6 grid grid-cols-3 gap-3">
          <Skeleton className="h-20 rounded-lg" />
          <Skeleton className="h-20 rounded-lg" />
          <Skeleton className="h-20 rounded-lg" />
        </div>
        <Skeleton className="mt-4 h-48 w-full rounded-lg" />
      </div>
    );
  }

  if (isError || !computer) {
    return (
      <div className="flex h-full flex-col items-center justify-center text-muted-foreground">
        <Monitor className="h-10 w-10 text-muted-foreground/30" />
        <p className="mt-3 text-sm">{t(($) => $.detail.not_found_title)}</p>
        <p className="mt-1 text-xs text-muted-foreground/70">
          {t(($) => $.detail.not_found_hint)}
        </p>
      </div>
    );
  }

  return <ComputerDetail computer={computer} />;
}
