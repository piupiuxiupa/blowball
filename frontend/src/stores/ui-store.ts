import { create } from 'zustand';

export type AgentStatus = 'idle' | 'running' | 'tool_call' | 'error';

interface UIState {
  activeSessionId: string | null;
  activeFilePath: string | null;
  sidebarCollapsed: boolean;
  streamingTokens: Record<string, string>;
  streamingReasoningTokens: Record<string, string>;
  agentStatus: Record<string, { agent: string; status: AgentStatus }>;

  setActiveSession: (id: string | null) => void;
  setActiveFile: (path: string | null) => void;
  toggleSidebar: () => void;
  appendToken: (sessionId: string, token: string) => void;
  clearStreaming: (sessionId: string) => void;
  appendReasoningToken: (sessionId: string, token: string) => void;
  clearStreamingReasoning: (sessionId: string) => void;
  setAgentStatus: (sessionId: string, agent: string, status: AgentStatus) => void;
}

export const useUIStore = create<UIState>((set) => ({
  activeSessionId: null,
  activeFilePath: null,
  sidebarCollapsed: false,
  streamingTokens: {},
  streamingReasoningTokens: {},
  agentStatus: {},

  setActiveSession: (id) => set({ activeSessionId: id }),
  setActiveFile: (path) => set({ activeFilePath: path }),
  toggleSidebar: () => set((state) => ({ sidebarCollapsed: !state.sidebarCollapsed })),
  appendToken: (sessionId, token) =>
    set((state) => ({
      streamingTokens: {
        ...state.streamingTokens,
        [sessionId]: (state.streamingTokens[sessionId] ?? '') + token,
      },
    })),
  clearStreaming: (sessionId) =>
    set((state) => {
      const next = { ...state.streamingTokens };
      delete next[sessionId];
      return { streamingTokens: next };
    }),
  appendReasoningToken: (sessionId, token) =>
    set((state) => ({
      streamingReasoningTokens: {
        ...state.streamingReasoningTokens,
        [sessionId]: (state.streamingReasoningTokens[sessionId] ?? '') + token,
      },
    })),
  clearStreamingReasoning: (sessionId) =>
    set((state) => {
      const next = { ...state.streamingReasoningTokens };
      delete next[sessionId];
      return { streamingReasoningTokens: next };
    }),
  setAgentStatus: (sessionId, agent, status) =>
    set((state) => ({
      agentStatus: {
        ...state.agentStatus,
        [sessionId]: { agent, status },
      },
    })),
}));
