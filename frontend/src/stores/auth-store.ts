import { create } from 'zustand';
import { persist } from 'zustand/middleware';

interface AuthState {
  token: string | null;
  expire: number | null;
  isAuthenticated: boolean;
  hydrated: boolean;
  login: (token: string, expire: number) => void;
  logout: () => void;
  finishHydration: () => void;
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set, get) => ({
      token: null,
      expire: null,
      isAuthenticated: false,
      hydrated: false,
      login: (token, expire) => set({ token, expire, isAuthenticated: true }),
      logout: () => set({ token: null, expire: null, isAuthenticated: false }),
      finishHydration: () => {
        const state = get();
        if (!state) return;
        const expired = state.expire ? Date.now() / 1000 > state.expire : false;
        if (state.token && !expired) {
          set({ isAuthenticated: true, hydrated: true });
        } else {
          set({ token: null, expire: null, isAuthenticated: false, hydrated: true });
        }
      },
    }),
    {
      name: 'blowball-auth',
      partialize: (state) => ({ token: state.token, expire: state.expire }),
    }
  )
);
