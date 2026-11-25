import { Navigate, Route, Routes } from "react-router";
import { ErrorBoundary } from "@/components/errorBoundary";
import { AppShell } from "@/components/layout/AppShell";
import { Skeleton } from "@/components/ui/primitives";
import { useSession } from "@/lib/session";
import { AgentsPage } from "@/pages/AgentsPage";
import { EvidencePage } from "@/pages/EvidencePage";
import { InboxPage } from "@/pages/InboxPage";
import { LoginPage, TenantPickerPage } from "@/pages/LoginPage";
import { MandatesPage } from "@/pages/MandatesPage";
import { PeoplePage } from "@/pages/PeoplePage";

function RequireSession({ children }: { children: React.ReactNode }) {
  const session = useSession();
  if (session.loading) {
    return (
      <div className="p-10">
        <Skeleton className="h-10 w-64" />
      </div>
    );
  }
  if (!session.signedIn) return <Navigate to="/login" replace />;
  if (!session.tenant) return <TenantPickerPage />;
  return <>{children}</>;
}

export default function App() {
  return (
    <ErrorBoundary>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route
          element={
            <RequireSession>
              <AppShell />
            </RequireSession>
          }
        >
          <Route index element={<Navigate to="/inbox" replace />} />
          <Route path="/inbox" element={<InboxPage />} />
          <Route path="/inbox/:id" element={<InboxPage />} />
          <Route path="/mandates" element={<MandatesPage />} />
          <Route path="/agents" element={<AgentsPage />} />
          <Route path="/people" element={<PeoplePage />} />
          <Route path="/evidence" element={<EvidencePage />} />
          <Route path="*" element={<Navigate to="/inbox" replace />} />
        </Route>
      </Routes>
    </ErrorBoundary>
  );
}
