import type { ReactNode } from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

// ApiError is re-exported from @multica/core/api; we mock the api module
// itself but still need a real ApiError class so `e instanceof ApiError`
// in the polling catch behaves the way it does at runtime.
const ApiError = vi.hoisted(() => {
  class ApiError extends Error {
    readonly status: number;
    readonly statusText: string;
    readonly body?: unknown;
    constructor(message: string, status: number, statusText = "", body?: unknown) {
      super(message);
      this.name = "ApiError";
      this.status = status;
      this.statusText = statusText;
      this.body = body;
    }
  }
  return ApiError;
});

const mockBeginInstall = vi.hoisted(() => vi.fn());
const mockGetStatus = vi.hoisted(() => vi.fn());
const mockDeleteInstallation = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());

type MemberRole = "owner" | "admin" | "member" | "guest";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as MemberRole }],
}));
const installationsRef = vi.hoisted(() => ({
  current: {
    installations: [] as unknown[],
    configured: true,
    install_supported: true,
  },
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current, isLoading: false };
    if (key.includes("installations")) {
      return { data: installationsRef.current, isLoading: false };
    }
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({
    invalidateQueries: mockInvalidate,
  }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/lark", () => ({
  larkInstallationsOptions: () => ({
    queryKey: ["lark", "installations"],
    queryFn: vi.fn(),
  }),
  larkKeys: { installations: (wsId: string) => ["lark", "installations", wsId] },
}));

vi.mock("@multica/core/api", () => ({
  api: {
    beginLarkInstall: mockBeginInstall,
    getLarkInstallStatus: mockGetStatus,
    deleteLarkInstallation: mockDeleteInstallation,
  },
  ApiError,
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string } }) => unknown) =>
      sel ? sel({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("sonner", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
    message: vi.fn(),
  },
}));

// react-qr-code renders SVG that jsdom doesn't fully support — a stub
// keeps the dialog DOM compact and lets us assert on the surrounding
// chrome (status text, buttons) without QR mechanics.
vi.mock("react-qr-code", () => ({
  default: ({ value }: { value: string }) => (
    <span data-testid="qr-code" data-value={value} />
  ),
}));

import { LarkAgentBindButton } from "./lark-tab";

const TEST_RESOURCES = {
  en: { common: enCommon, settings: enSettings },
};

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function resetFixtures() {
  vi.clearAllMocks();
  membersRef.current = [{ user_id: "user-1", role: "owner" }];
  installationsRef.current = {
    installations: [],
    configured: true,
    install_supported: true,
  };
}

describe("LarkAgentBindButton (CTA gate)", () => {
  beforeEach(resetFixtures);

  it("renders the bind CTA when the viewer is a workspace owner and install is supported", () => {
    render(<LarkAgentBindButton agentId="agent-1" agentName="Bot" />, {
      wrapper: I18nWrapper,
    });
    expect(screen.getByRole("button", { name: /Bind to Lark/i })).toBeTruthy();
  });

  it("renders the bind CTA when the viewer is a workspace admin", () => {
    membersRef.current = [{ user_id: "user-1", role: "admin" }];
    render(<LarkAgentBindButton agentId="agent-1" agentName="Bot" />, {
      wrapper: I18nWrapper,
    });
    expect(screen.getByRole("button", { name: /Bind to Lark/i })).toBeTruthy();
  });

  it("hides the bind CTA for a non-admin agent owner (matches backend admin gate)", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    const { container } = render(
      <LarkAgentBindButton agentId="agent-1" agentName="Bot" />,
      { wrapper: I18nWrapper },
    );
    expect(container.querySelector("button")).toBeNull();
  });

  it("hides the bind CTA when the device-flow install path is not wired on the server", () => {
    installationsRef.current.install_supported = false;
    const { container } = render(
      <LarkAgentBindButton agentId="agent-1" agentName="Bot" />,
      { wrapper: I18nWrapper },
    );
    expect(container.querySelector("button")).toBeNull();
  });
});

describe("LarkInstallDialog (polling terminal errors)", () => {
  beforeEach(() => {
    resetFixtures();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mockBeginInstall.mockResolvedValue({
      session_id: "sess-1",
      qr_code_url: "https://accounts.feishu.cn/oauth/v1/device?u=abc",
      expires_in_seconds: 300,
      poll_interval_seconds: 2,
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  async function openDialog() {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<LarkAgentBindButton agentId="agent-1" agentName="Bot" />, {
      wrapper: I18nWrapper,
    });
    await user.click(screen.getByRole("button", { name: /Bind to Lark/i }));
    // Let the begin-session promise resolve and the QR render.
    await waitFor(() => {
      expect(screen.getByTestId("qr-code")).toBeTruthy();
    });
  }

  it("falls into a terminal session_lost error state when status polling 404s instead of looping forever", async () => {
    mockGetStatus.mockRejectedValue(
      new ApiError("install session not found", 404, "Not Found"),
    );

    await openDialog();
    // Drive the polling timer (intervalMs = max(2000, 2*1000)) and let
    // the rejected promise propagate into the catch.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });

    await waitFor(() => {
      expect(
        screen.getByText(
          /Install session expired or was lost\. Scan again to start over\./i,
        ),
      ).toBeTruthy();
    });
    expect(screen.getByRole("button", { name: /Scan again/i })).toBeTruthy();
    // The dialog renders multiple Close affordances (footer button + the
    // built-in dialog dismiss); we only need to confirm at least one is
    // mounted alongside the retry button.
    expect(screen.getAllByRole("button", { name: /Close/i }).length).toBeGreaterThan(0);
  });

  it("treats 403 as a terminal forbidden error state (no infinite retry on revoked permission)", async () => {
    mockGetStatus.mockRejectedValue(
      new ApiError("forbidden", 403, "Forbidden"),
    );

    await openDialog();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });

    await waitFor(() => {
      expect(
        screen.getByText(
          /You no longer have permission to install Lark Bots in this workspace/i,
        ),
      ).toBeTruthy();
    });
    // Drive another full poll interval — the polling loop must NOT
    // schedule a follow-up fetch after a terminal 4xx.
    const callsAfterTerminal = mockGetStatus.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(mockGetStatus.mock.calls.length).toBe(callsAfterTerminal);
  });
});
