"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  registerForWorkspaceRehydration,
} from "../../platform/workspace-storage";
import { defaultStorage } from "../../platform/storage";

export type AgentsScope = "mine" | "all";

export interface AgentsViewState {
  scope: AgentsScope;
  setScope: (scope: AgentsScope) => void;
}

export const useAgentsViewStore = create<AgentsViewState>()(
  persist(
    (set) => ({
      scope: "mine",
      setScope: (scope) => set({ scope }),
    }),
    {
      name: "multica_agents_view",
      storage: createJSONStorage(() => createWorkspaceAwareStorage(defaultStorage)),
      partialize: (state) => ({ scope: state.scope }),
    },
  ),
);

registerForWorkspaceRehydration(() => useAgentsViewStore.persist.rehydrate());
