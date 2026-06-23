import { useEffect } from 'react';
import { BrowserRouter, Routes, Route, Navigate, useNavigate, useLocation } from 'react-router';
import { useAuthStore } from '@/stores/auth-store';
import { LoginPage } from '@/pages/login-page';
import { MainPage } from '@/pages/main-page';
import { getRouterBasename } from '@/lib/config';

function AuthGuard({ children }: { children: React.ReactNode }) {
  const { isAuthenticated, expire, hydrated, finishHydration } = useAuthStore();
  const navigate = useNavigate();
  const location = useLocation();

  useEffect(() => {
    finishHydration();
  }, [finishHydration]);

  useEffect(() => {
    if (!hydrated) return;
    const expired = expire ? Date.now() / 1000 > expire : false;
    if (!isAuthenticated || expired) {
      navigate('/login', { replace: true, state: { from: location.pathname } });
    }
  }, [isAuthenticated, expire, hydrated, navigate, location.pathname]);

  if (!hydrated) {
    return (
      <div className="flex h-screen items-center justify-center text-sm text-muted-foreground">
        加载中…
      </div>
    );
  }
  if (!isAuthenticated) return null;
  return <>{children}</>;
}

export function App() {
  return (
    <BrowserRouter basename={getRouterBasename()}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route
          path="/"
          element={
            <AuthGuard>
              <MainPage />
            </AuthGuard>
          }
        />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  );
}
