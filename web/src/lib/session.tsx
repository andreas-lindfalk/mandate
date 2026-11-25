import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import * as React from "react";
import type { TenantMembership } from "@/gen/mandate/v1/auth_pb";
import { api, sessionStore } from "@/lib/api";

type Session = {
  /** True until the first Me call has answered. */
  loading: boolean;
  /** Signed in, by token or by cookie. */
  signedIn: boolean;
  email: string;
  name: string;
  devLogin: boolean;
  oidc: boolean;
  tenants: TenantMembership[];
  tenant: TenantMembership | null;
  isOwner: boolean;
  login: (email: string, name: string) => Promise<void>;
  logout: () => Promise<void>;
  selectTenant: (id: string) => void;
};

const SessionContext = React.createContext<Session | null>(null);

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const [tenantId, setTenantId] = React.useState(sessionStore.tenantId);
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
  const tenant = tenants.find((t) => t.tenantId === tenantId) ?? (tenants.length === 1 ? tenants[0] : null);
  if (tenant && tenant.tenantId !== sessionStore.tenantId) sessionStore.setTenant(tenant.tenantId);

  const value: Session = {
    loading: me.isPending || methods.isPending,
    signedIn: !!me.data,
    email: me.data?.email ?? "",
    name: me.data?.name ?? "",
    devLogin: methods.data?.devLogin ?? false,
    oidc: methods.data?.oidc ?? false,
    tenants,
    tenant,
    isOwner: tenant?.role === "owner",
    async login(email, name) {
      const response = await api.auth.devLogin({ email, name });
      queryClient.clear();
      sessionStore.setToken(response.token);
      sessionStore.setTenant(null);
      setTenantId(null);
      setGeneration((g) => g + 1);
    },
    async logout() {
      try {
        await api.auth.logout({});
      } finally {
        sessionStore.setToken(null);
        sessionStore.setTenant(null);
        setTenantId(null);
        queryClient.clear();
        setGeneration((g) => g + 1);
      }
    },
    selectTenant(id) {
      sessionStore.setTenant(id);
      setTenantId(id);
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "me" && query.queryKey[0] !== "sign-in-methods" });
    },
  };
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession() {
  const session = React.useContext(SessionContext);
  if (!session) throw new Error("useSession outside SessionProvider");
  return session;
}
