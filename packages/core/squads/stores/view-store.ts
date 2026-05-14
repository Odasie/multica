"use client";

import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  registerForWorkspaceRehydration,
} from "../../platform/workspace-storage";
import { defaultStorage } from "../../platform/storage";

export type SquadsScope = "mine" | "all";

export interface SquadsViewState {
  scope: SquadsScope;
  setScope: (scope: SquadsScope) => void;
}

export const useSquadsViewStore = create<SquadsViewState>()(
  persist(
    (set) => ({
      scope: "mine",
      setScope: (scope) => set({ scope }),
    }),
    {
      name: "multica_squads_view",
      storage: createJSONStorage(() => createWorkspaceAwareStorage(defaultStorage)),
      partialize: (state) => ({ scope: state.scope }),
    },
  ),
);

registerForWorkspaceRehydration(() => useSquadsViewStore.persist.rehydrate());
