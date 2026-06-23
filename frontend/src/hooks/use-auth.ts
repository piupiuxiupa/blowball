import { useMutation } from '@tanstack/react-query';
import { apiPost, type LoginRequest, type LoginResponse } from '@/lib/api';
import { useAuthStore } from '@/stores/auth-store';

export function useAuth() {
  const { token, isAuthenticated, login, logout } = useAuthStore();

  const loginMutation = useMutation({
    mutationFn: async (credentials: LoginRequest) => {
      const response = await apiPost<LoginResponse>('/api/v1/auth/login', {
        body: credentials,
        token: null,
      });
      return response;
    },
    onSuccess: (data) => {
      login(data.access_token, data.expire);
    },
  });

  return {
    token,
    isAuthenticated,
    login: loginMutation.mutateAsync,
    isLoggingIn: loginMutation.isPending,
    loginError: loginMutation.error,
    logout,
  };
}
