import { Code, ConnectError } from "@connectrpc/connect";
import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import { useNavigate } from "react-router";
import type { TenantMembership } from "@/gen/mandate/v1/auth_pb";
import { api, sessionStore } from "@/lib/api";

type Session = {
  /** True until the first Me call has answered. */
  loading: boolean;
  /**
   * Why the session or the sign-in methods are unknown: a call failed and
   * nothing from an earlier load is at hand. Being signed out is not an
   * error. While it is set, signedIn and the sign-in methods say nothing.
   */
  error: unknown;
  /** Asks again after an error. */
  retry: () => void;
  /** Signed in, by token or by cookie. */
  signedIn: boolean;
  email: string;
  name: string;
  devLogin: boolean;
  oidc: boolean;
  /** Nobody has set pgmandate up yet: the setup link is in the server's log. */
  setupPending: boolean;
  tenants: TenantMembership[];
  tenant: TenantMembership | null;
  isOwner: boolean;
  login: (email: string, name: string) => Promise<void>;
  /** Picks up a session the server just set as a cookie, as after a link. */
  signedInByCookie: () => void;
  logout: () => Promise<void>;
  selectTenant: (id: string) => void;
};

const SessionContext = React.createContext<Session | null>(null);

/** Drops what was read in one tenant; who is signed in stays. */
function removeTenantQueries(queryClient: QueryClient) {
  queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "me" && query.queryKey[0] !== "sign-in-methods" });
}

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [tenantId, setTenantId] = React.useState(sessionStore.tenantId);
  // The tenant whose data is in the query cache. Nothing is cached before a
  // tenant is resolved, so a tenant remembered from an earlier visit counts
  // for nothing here: a link opened in a fresh page keeps its place.
  const cachedTenant = React.useRef<string | null>(null);
  const [generation, setGeneration] = React.useState(0);

  // Me answers for a token and for a cookie alike; Unauthenticated means
  // there is no session, which is a normal state, not an error.
  const me = useQuery({
    queryKey: ["me", generation],
    retry: false,
    queryFn: async () => {
      try {
        return await api.auth.me({});
      } catch (error) {
        if (ConnectError.from(error).code === Code.Unauthenticated) return null;
        throw error;
      }
    },
  });

  // How to sign in is public: the sign-in page needs it before any session.
  const methods = useQuery({ queryKey: ["sign-in-methods"], staleTime: 60_000, queryFn: () => api.auth.signInMethods({}) });

  const tenants = me.data?.tenants ?? [];
  const tenant = tenants.find((t) => t.tenantId === (tenantId ?? sessionStore.tenantId)) ?? (tenants.length === 1 ? tenants[0] : null);
  const resolved = tenant?.tenantId ?? null;
  // A tenant picked without a choice (the only one, or the only one left
  // after a removal) becomes the chosen one, so a second tenant added later
  // does not send the person to the picker. The header goes with the
  // requests the pages send once this render is committed, so the store is
  // set now and not in an effect.
  if (resolved && resolved !== sessionStore.tenantId) sessionStore.setTenant(resolved);
  if (resolved && resolved !== tenantId) setTenantId(resolved);

  // If the cache holds another tenant's data, it goes, as when a person
  // switches: the open page and its lists belong to the old tenant.
  React.useEffect(() => {
    if (!resolved || cachedTenant.current === resolved) return;
    const switched = cachedTenant.current !== null;
    cachedTenant.current = resolved;
    if (!switched) return;
    removeTenantQueries(queryClient);
    navigate("/inbox", { replace: true });
  }, [resolved, queryClient, navigate]);

  const value: Session = {
    loading: me.isPending || methods.isPending,
    error: (me.data === undefined ? me.error : null) ?? (methods.data === undefined ? methods.error : null),
    retry() {
      if (me.data === undefined) void me.refetch();
      if (methods.data === undefined) void methods.refetch();
    },
    signedIn: !!me.data,
    email: me.data?.email ?? "",
    name: me.data?.name ?? "",
    devLogin: methods.data?.devLogin ?? false,
    oidc: methods.data?.oidc ?? false,
    setupPending: methods.data?.setupPending ?? false,
    tenants,
    tenant,
    isOwner: tenant?.role === "owner",
    async login(email, name) {
      const response = await api.auth.devLogin({ email, name });
      queryClient.clear();
      sessionStore.setToken(response.token);
      sessionStore.setTenant(null);
      cachedTenant.current = null;
      setTenantId(null);
      setGeneration((g) => g + 1);
    },
    signedInByCookie() {
      queryClient.clear();
      sessionStore.setToken(null);
      sessionStore.setTenant(null);
      cachedTenant.current = null;
      setTenantId(null);
      setGeneration((g) => g + 1);
    },
    async logout() {
      try {
        await api.auth.logout({});
      } finally {
        sessionStore.setToken(null);
        sessionStore.setTenant(null);
        cachedTenant.current = null;
        setTenantId(null);
        queryClient.clear();
        setGeneration((g) => g + 1);
      }
    },
    selectTenant(id) {
      sessionStore.setTenant(id);
      cachedTenant.current = id;
      setTenantId(id);
      removeTenantQueries(queryClient);
    },
  };
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession() {
  const session = React.useContext(SessionContext);
  if (!session) throw new Error("useSession outside SessionProvider");
  return session;
}
