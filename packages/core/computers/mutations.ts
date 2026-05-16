import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import { runtimeKeys } from "../runtimes/queries";
import { computerKeys } from "./queries";

// useDeleteComputer wraps `DELETE /api/computers/:id` (RFC v6.1 §6.3). The
// server refuses with 409 when any runtime under the computer still has
// active agent tasks; the body carries `active_agents: number` so the UI
// can show a precise count without parsing a human-readable message.
//
// We also invalidate runtimeKeys because every runtime hosted by this
// daemon is implicitly gone — the runtime list page should reflect that
// without a separate refetch.
export function useDeleteComputer(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (computerId: string) => api.deleteComputer(computerId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: computerKeys.all(wsId) });
      qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
    },
  });
}

/** Returns the `active_agents` count from a 409 error body, or null. */
export function activeAgentsFromError(err: unknown): number | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body as Record<string, unknown> | null | undefined;
  if (!body) return null;
  const n = body.active_agents;
  return typeof n === "number" ? n : null;
}
