import { Code, ConnectError } from "@connectrpc/connect";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";
import { Toaster } from "sonner";
import App from "@/App";
import { SessionProvider } from "@/lib/session";
import "@/index.css";

// A session can end while the app is open: it expires, the person signs out
// in another tab, or the server stops accepting it. A call refused as
// Unauthenticated asks Me again, which answers signed out (it maps
// Unauthenticated to null, so this does not loop), and RequireSession then
// sends the person to sign in and back to the page they were on.
function onError(error: unknown) {
  if (ConnectError.from(error).code === Code.Unauthenticated) void queryClient.invalidateQueries({ queryKey: ["me"] });
}

const queryClient: QueryClient = new QueryClient({
  queryCache: new QueryCache({ onError }),
  mutationCache: new MutationCache({ onError }),
  defaultOptions: { queries: { staleTime: 5_000, refetchOnWindowFocus: true, retry: 1 } },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <SessionProvider>
          <App />
          <Toaster
            position="bottom-right"
            toastOptions={{
              classNames: {
                toast: "!bg-espresso !text-cream !border-espresso !rounded-[10px] !font-sans",
                description: "!text-cream/70",
              },
            }}
          />
        </SessionProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
