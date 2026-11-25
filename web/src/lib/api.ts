import { createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AuthService } from "@/gen/mandate/v1/auth_pb";
import { ActionService } from "@/gen/mandate/v1/actions_pb";
import { AgentService } from "@/gen/mandate/v1/agents_pb";
import { EvidenceService } from "@/gen/mandate/v1/evidence_pb";
import { MandateService } from "@/gen/mandate/v1/mandates_pb";
import { NotifierService } from "@/gen/mandate/v1/notifiers_pb";
import { PersonService } from "@/gen/mandate/v1/persons_pb";
import { ProposalService } from "@/gen/mandate/v1/proposals_pb";

const TOKEN_KEY = "mandate.token";
const TENANT_KEY = "mandate.tenant";

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    /* private mode: the session lives in memory only */
  }
}

/**
 * The session: a bearer token from dev login (kept in localStorage), or the
 * cookie the OIDC callback set, which the browser sends by itself. The
 * chosen tenant is remembered per browser.
 */
export const sessionStore = {
  token: read(TOKEN_KEY),
  tenantId: read(TENANT_KEY),
  setToken(token: string | null) {
    this.token = token;
    write(TOKEN_KEY, token);
  },
  setTenant(tenantId: string | null) {
    this.tenantId = tenantId;
    write(TENANT_KEY, tenantId);
  },
};

const sessionHeaders: Interceptor = (next) => async (req) => {
  if (sessionStore.token) req.header.set("Authorization", `Bearer ${sessionStore.token}`);
  if (sessionStore.tenantId) req.header.set("Mandate-Tenant", sessionStore.tenantId);
  return next(req);
};

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  interceptors: [sessionHeaders],
  fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin" }),
});

export const api = {
  auth: createClient(AuthService, transport),
  persons: createClient(PersonService, transport),
  agents: createClient(AgentService, transport),
  actions: createClient(ActionService, transport),
  mandates: createClient(MandateService, transport),
  proposals: createClient(ProposalService, transport),
  evidence: createClient(EvidenceService, transport),
  notifiers: createClient(NotifierService, transport),
};
