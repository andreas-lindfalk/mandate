import * as React from "react";
import { Navigate, Route, Routes, useLocation } from "react-router";
import { ErrorBoundary } from "@/components/errorBoundary";
import { AppShell } from "@/components/layout/AppShell";
import { Skeleton } from "@/components/ui/primitives";
import { useSession } from "@/lib/session";
import { AgentsPage } from "@/pages/AgentsPage";
import { EvidencePage } from "@/pages/EvidencePage";
import { InboxPage } from "@/pages/InboxPage";
import { SetupPage, SignInLinkPage } from "@/pages/LinkPages";
import { LoginPage, TenantPickerPage } from "@/pages/LoginPage";
import { MandatesPage } from "@/pages/MandatesPage";
import { PeoplePage } from "@/pages/PeoplePage";

// Where a signed-out visit was going, such as a proposal linked from a chat
// notification, so sign-in (dev login or the provider's round trip) ends
// there and not on the front page. Only a path on this site is kept.
const returnKey = "pgmandate.returnTo";

function remember(path: string) {
  try {
    if (path.startsWith("/") && !path.startsWith("//") && path !== "/login") sessionStorage.setItem(returnKey, path);
  } catch {
    // Without storage, sign-in ends on the front page.
  }
}

// Read during render, cleared in an effect once the page is there: React may
// render twice (StrictMode) or throw a render away, and a read that cleared
// would lose the way back.
function readReturn(): string | null {
  try {
    const path = sessionStorage.getItem(returnKey);
    return path && path.startsWith("/") && !path.startsWith("//") ? path : null;
  } catch {
    return null;
  }
}

function clearReturn() {
  try {
    sessionStorage.removeItem(returnKey);
  } catch {
    // Nothing to clear.
  }
}

function RequireSession({ children }: { children: React.ReactNode }) {
  const session = useSession();
  const location = useLocation();
  const here = location.pathname + location.search;
  const back = session.signedIn && session.tenant ? readReturn() : null;
  React.useEffect(() => {
    if (back && back === here) clearReturn();
  }, [back, here]);
  if (session.loading) {
    return (
      <div className="p-10">
        <Skeleton className="h-10 w-64" />
      </div>
    );
  }
  if (!session.signedIn) {
    remember(here);
    return <Navigate to="/login" replace />;
  }
  if (!session.tenant) return <TenantPickerPage />;
  if (back && back !== here) return <Navigate to={back} replace />;
  return <>{children}</>;
}

export default function App() {
  return (
    <ErrorBoundary>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/setup" element={<SetupPage />} />
        <Route path="/sign-in" element={<SignInLinkPage />} />
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
