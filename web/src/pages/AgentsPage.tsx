import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Bot, Check, Copy, KeyRound, Plus, Webhook } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogTrigger } from "@/components/ui/dialog";
import { Badge, Card, EmptyState, ErrorState, Field, Input, SectionLabel, Skeleton, Textarea } from "@/components/ui/primitives";
import { Select } from "@/components/ui/select";
import type { Action } from "@/gen/mandate/v1/actions_pb";
import type { Agent, Token } from "@/gen/mandate/v1/agents_pb";
import { api } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { errorText } from "@/lib/errors";
import { cn } from "@/lib/utils";
import { formatDateTime, formatRelative, riskLabel, riskTone, units } from "@/lib/format";
import { useSession } from "@/lib/session";

function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = React.useState(false);
  return (
    <Button
      type="button"
      variant="secondary"
      size="sm"
      aria-label={label}
      onClick={async () => {
        if (await copyText(text)) {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 2000);
        } else {
          toast.error("Could not copy", { description: "Select the text and copy it yourself." });
        }
      }}
    >
      {copied ? <Check /> : <Copy />} {copied ? "Copied" : "Copy"}
    </Button>
  );
}

function Snippet({ title, text }: { title: string; text: string }) {
  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between gap-3">
        <p className="text-[13px] font-medium">{title}</p>
        <CopyButton text={text} label={`Copy ${title.toLowerCase()}`} />
      </div>
      <pre className="overflow-x-auto rounded-[var(--radius-control)] bg-espresso px-4 py-3 font-mono text-[12px] leading-relaxed text-cream">{text}</pre>
    </div>
  );
}

/** The agent token, shown once, with ready-made configuration for common agents. */
function NewToken({ secret, onDone }: { secret: string; onDone: () => void }) {
  const url = `${window.location.origin}/mcp`;
  const claudeCode = `claude mcp add --transport http pgmandate ${url} \\\n  --header "Authorization: Bearer ${secret}"`;
  const json = JSON.stringify({ mcpServers: { pgmandate: { type: "http", url, headers: { Authorization: `Bearer ${secret}` } } } }, null, 2);
  return (
    <Card className="border-accent/60 p-6">
      <div className="flex items-start gap-3">
        <KeyRound className="mt-0.5 size-5 shrink-0 text-accent" aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="font-serif text-[20px]">The token is ready</p>
          <p className="mt-1 text-[14px] text-muted">Copy it now. It is shown this once; pgmandate keeps only its hash.</p>
        </div>
      </div>
      <div className="mt-5 flex flex-col gap-5">
        <Snippet title="Token" text={secret} />
        <Snippet title="Claude Code" text={claudeCode} />
        <Snippet title="Other agents (MCP configuration)" text={json} />
      </div>
      <div className="mt-6 flex justify-end">
        <Button onClick={onDone}>I have saved it</Button>
      </div>
    </Card>
  );
}

function tokenState(token: Token): { label: string; tone: "success" | "neutral" | "warning" } {
  if (token.revokedAt) return { label: "Revoked", tone: "neutral" };
  if (token.expiresAt && timestampDate(token.expiresAt) < new Date()) return { label: "Expired", tone: "warning" };
  return { label: "Active", tone: "success" };
}

function AgentCard({ agent, isOwner, onToken }: { agent: Agent; isOwner: boolean; onToken: (secret: string) => void }) {
  const queryClient = useQueryClient();
  const tokens = useQuery({ queryKey: ["tokens", agent.id], queryFn: () => api.agents.listTokens({ agentId: agent.id }) });
  const [days, setDays] = React.useState("90");
  const issue = useMutation({
    mutationFn: () => api.agents.issueToken({ agentId: agent.id, days: Number(days) }),
    gcTime: 0,
    onSuccess: (response) => {
      queryClient.invalidateQueries({ queryKey: ["tokens", agent.id] });
      onToken(response.secret);
    },
    onError: (error) => toast.error("Could not issue a token", { description: errorText(error) }),
  });
  const revokeToken = useMutation({
    mutationFn: (id: string) => api.agents.revokeToken({ id }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["tokens", agent.id] }),
    onError: (error) => toast.error("Could not revoke", { description: errorText(error) }),
  });
  const revokeAgent = useMutation({
    mutationFn: () => api.agents.revokeAgent({ id: agent.id }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      toast.success(`${agent.name} is revoked`);
    },
    onError: (error) => toast.error("Could not revoke", { description: errorText(error) }),
  });
  const [confirming, setConfirming] = React.useState(false);
  const [revoking, setRevoking] = React.useState<string | null>(null);
  const [editingLimits, setEditingLimits] = React.useState(false);
  const [perMinute, setPerMinute] = React.useState(String(agent.maxPerMinute));
  const [perHour, setPerHour] = React.useState(String(agent.maxPerHour));
  const minute = Number(perMinute);
  const hour = Number(perHour);
  const limitsProblem =
    !perMinute.trim() || !perHour.trim() || !Number.isInteger(minute) || !Number.isInteger(hour)
      ? "Give whole numbers."
      : minute < 1 || minute > 10000
        ? "A minute takes 1 to 10000."
        : hour < minute || hour > 100000
          ? "An hour takes at least the minute's number, and at most 100000."
          : "";
  const setLimits = useMutation({
    mutationFn: () => api.agents.setAgentLimits({ id: agent.id, maxPerMinute: Number(perMinute), maxPerHour: Number(perHour) }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      setEditingLimits(false);
      toast.success("Limits set");
    },
    onError: (error) => toast.error("Could not set the limits", { description: errorText(error) }),
  });
  const live = !agent.revokedAt;
  return (
    <Card className="p-5">
      <div className="flex flex-wrap items-center gap-2">
        <Bot className="size-4 text-accent" aria-hidden />
        <p className="font-medium">{agent.name}</p>
        {agent.department ? <Badge tone="neutral">{agent.department}</Badge> : null}
        {!live ? <Badge tone="neutral">revoked</Badge> : null}
        <span className="ml-auto text-[12px] text-muted">since {formatDateTime(agent.createdAt)}</span>
      </div>
      <p className="mt-1 font-mono text-[11px] text-muted">{agent.id}</p>
      {editingLimits ? (
        <form
          className="mt-3 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setLimits.mutate();
          }}
        >
          <Field label="Proposals a minute" htmlFor={`per-minute-${agent.id}`} className="w-40">
            <Input id={`per-minute-${agent.id}`} type="number" min={1} max={10000} value={perMinute} onChange={(e) => setPerMinute(e.target.value)} />
          </Field>
          <Field label="Proposals an hour" htmlFor={`per-hour-${agent.id}`} className="w-40">
            <Input id={`per-hour-${agent.id}`} type="number" min={1} max={100000} value={perHour} onChange={(e) => setPerHour(e.target.value)} />
          </Field>
          <Button type="submit" variant="secondary" loading={setLimits.isPending} disabled={!!limitsProblem} aria-describedby={limitsProblem ? `limits-problem-${agent.id}` : undefined}>
            Set limits
          </Button>
          <Button type="button" variant="ghost" onClick={() => setEditingLimits(false)}>
            Cancel
          </Button>
          {limitsProblem ? (
            <p id={`limits-problem-${agent.id}`} className="w-full text-[12px] text-danger-ink">
              {limitsProblem}
            </p>
          ) : null}
        </form>
      ) : (
        <p className="mt-1 text-[12.5px] text-muted">
          Up to {agent.maxPerMinute} proposals a minute and {agent.maxPerHour} an hour; the database refuses more.
          {isOwner && live ? (
            <Button
              variant="ghost"
              size="sm"
              className="ml-1"
              onClick={() => {
                setPerMinute(String(agent.maxPerMinute));
                setPerHour(String(agent.maxPerHour));
                setEditingLimits(true);
              }}
            >
              Change
            </Button>
          ) : null}
        </p>
      )}
      <ul className="mt-3 flex flex-col gap-1 text-[13px]">
        {(tokens.data?.tokens ?? []).map((token) => {
          const state = tokenState(token);
          return (
            <li key={token.id} className="flex flex-wrap items-center gap-2">
              <span className="font-mono">…{token.hint}</span>
              <Badge tone={state.tone}>{state.label}</Badge>
              <span className="text-muted">
                {state.label === "Active" ? `until ${formatDateTime(token.expiresAt)}` : ""} ·{" "}
                {token.lastUsedAt ? `used ${formatRelative(token.lastUsedAt)}` : "never used"}
              </span>
              {isOwner && state.label === "Active" ? (
                <Button
                  variant="ghost"
                  size="sm"
                  className="ml-auto"
                  onClick={() => (revoking === token.id ? revokeToken.mutate(token.id) : setRevoking(token.id))}
                >
                  {revoking === token.id ? "Revoke now, cutting the agent off" : "Revoke token"}
                </Button>
              ) : null}
            </li>
          );
        })}
      </ul>
      {isOwner && live ? (
        <div className="mt-4 flex flex-wrap items-end gap-2 border-t border-border pt-4">
          <Field label="Token valid for" htmlFor={`days-${agent.id}`} className="w-40">
            <Select
              id={`days-${agent.id}`}
              value={days}
              onValueChange={setDays}
              options={[
                { value: "30", label: "30 days" },
                { value: "90", label: "90 days" },
                { value: "365", label: "One year" },
              ]}
            />
          </Field>
          <Button variant="secondary" onClick={() => issue.mutate()} loading={issue.isPending}>
            <KeyRound /> Issue a token
          </Button>
          <div className="ml-auto flex gap-2">
            {confirming ? (
              <>
                <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
                <Button variant="danger" size="sm" onClick={() => revokeAgent.mutate()} loading={revokeAgent.isPending}>
                  Revoke the agent
                </Button>
              </>
            ) : (
              <Button variant="ghost" size="sm" onClick={() => setConfirming(true)}>
                Revoke agent
              </Button>
            )}
          </div>
        </div>
      ) : null}
    </Card>
  );
}

function NewAgent({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = React.useState("");
  const [department, setDepartment] = React.useState("");
  const create = useMutation({
    mutationFn: () => api.agents.createAgent({ name: name.trim(), department: department.trim() }),
    onSuccess: () => {
      setName("");
      setDepartment("");
      onCreated();
    },
    onError: (error) => toast.error("Could not register", { description: errorText(error) }),
  });
  return (
    <Card className="p-5">
      <SectionLabel>New agent</SectionLabel>
      <form
        className="mt-3 grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <Field label="Name" htmlFor="agent-name">
          <Input id="agent-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="support-bot" maxLength={80} />
        </Field>
        <Field label="Department" htmlFor="agent-department">
          <Input
            id="agent-department"
            value={department}
            onChange={(e) => setDepartment(e.target.value)}
            placeholder="optional"
            maxLength={100}
            aria-describedby="agent-department-hint"
          />
        </Field>
        <Button type="submit" disabled={!name.trim()} loading={create.isPending}>
          <Plus /> Register
        </Button>
        {/* Under the whole row, so the fields and the button stay aligned. */}
        <p id="agent-department-hint" className="text-xs text-muted sm:col-span-3">
          The department is a label for finding things. It limits nothing yet.
        </p>
      </form>
    </Card>
  );
}

function DefineActionDialog({ existing, allowBuiltin, onDone }: { existing?: Action; allowBuiltin: boolean; onDone: (secret: string) => void }) {
  const [open, setOpen] = React.useState(false);
  const [key, setKey] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [risk, setRisk] = React.useState("low");
  const [cost, setCost] = React.useState("1");
  const [mayExecute, setMayExecute] = React.useState(false);
  const [connector, setConnector] = React.useState("webhook");
  const [url, setUrl] = React.useState("");
  const [schema, setSchema] = React.useState("");
  const [department, setDepartment] = React.useState("");
  const [rotate, setRotate] = React.useState(false);
  // The schema's error shows once the field has been left or a save tried,
  // not on a dialog that has only just opened.
  const [schemaTouched, setSchemaTouched] = React.useState(false);
  // Every opening starts from the action as it is saved, so edits that
  // were cancelled never come back to be saved by accident.
  const reset = () => {
    setKey(existing?.key ?? "");
    setDescription(existing?.description ?? "");
    setRisk(existing?.risk ?? "low");
    setCost(String(existing?.costUnits ?? 1));
    setMayExecute(existing?.mayExecute ?? false);
    setConnector(existing?.connector ?? "webhook");
    setUrl(existing?.connectorUrl ?? "");
    setSchema(existing?.schemaJson ? JSON.stringify(JSON.parse(existing.schemaJson), null, 2) : "");
    setDepartment(existing?.department ?? "");
    setRotate(false);
    setSchemaTouched(false);
  };
  const onOpenChange = (next: boolean) => {
    if (next) reset();
    setOpen(next);
  };
  const schemaError = (() => {
    if (!schema.trim()) return connector === "webhook" ? "Required for a webhook." : "";
    try {
      const parsed: unknown = JSON.parse(schema);
      return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? "" : "A schema is a JSON object.";
    } catch {
      return "Not valid JSON.";
    }
  })();
  const highWebhook = connector === "webhook" && risk === "high";
  const define = useMutation({
    mutationFn: () =>
      api.actions.defineAction({
        key: key.trim(),
        description,
        risk,
        costUnits: BigInt(Number(cost) || 0),
        mayExecute: mayExecute && !highWebhook,
        connector,
        connectorUrl: connector === "webhook" ? url.trim() : "",
        schemaJson: schema.trim(),
        department,
        rotateSecret: rotate,
      }),
    onSuccess: (response) => {
      toast.success(existing ? "Action updated" : "Action defined");
      setOpen(false);
      onDone(response.secret);
    },
    onError: (error) => toast.error("Could not define", { description: errorText(error) }),
  });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>
        {existing ? (
          <Button variant="ghost" size="sm">
            Edit
          </Button>
        ) : (
          <Button>
            <Plus /> Define an action
          </Button>
        )}
      </DialogTrigger>
      <DialogContent
        className="w-[min(820px,calc(100vw-32px))]"
        title={existing ? `Edit ${existing.key}` : "Define an action"}
        description="What an agent may propose, and the connector that carries a decision out."
      >
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            // A save shows what is wrong with the schema instead of sending it.
            setSchemaTouched(true);
            if (schemaError) return;
            define.mutate();
          }}
        >
          <div className="grid gap-x-6 gap-y-4 md:grid-cols-2">
            <div className="flex flex-col gap-4">
              <Field label="Key" htmlFor="action-key" hint="Dotted lower case, such as tickets.create.">
                <Input id="action-key" value={key} onChange={(e) => setKey(e.target.value)} disabled={!!existing} />
              </Field>
              <Field label="Description" htmlFor="action-description" hint="Heads each proposal in the inbox and is shown to agents. Say what happens, such as “Open a ticket in the helpdesk.”">
                <Input id="action-description" value={description} onChange={(e) => setDescription(e.target.value)} />
              </Field>
              <div className="grid grid-cols-2 gap-4">
                <Field label="Risk" htmlFor="action-risk" hint="High always waits for a person.">
                  <Select
                    id="action-risk"
                    value={risk}
                    onValueChange={setRisk}
                    options={[
                      { value: "low", label: "Low" },
                      { value: "medium", label: "Medium" },
                      { value: "high", label: "High" },
                    ]}
                  />
                </Field>
                <Field label="Cost, units" htmlFor="action-cost" hint="Counted against a mandate's budget.">
                  <Input id="action-cost" type="number" min={0} value={cost} onChange={(e) => setCost(e.target.value)} />
                </Field>
              </div>
              <label className={cn("flex items-start gap-2 text-[13.5px]", highWebhook && "text-muted")}>
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={mayExecute && !highWebhook}
                  disabled={highWebhook}
                  onChange={(e) => setMayExecute(e.target.checked)}
                />
                <span>
                  <span className="font-medium">Can be delegated.</span>{" "}
                  {highWebhook
                    ? "Not for high risk: a mandate never covers it, so a person always decides."
                    : "A mandate may let this be carried out without a person. Off: a person always decides."}
                </span>
              </label>
            </div>
            <div className="flex flex-col gap-4">
              <Field label="Connector" htmlFor="action-connector">
                <Select
                  id="action-connector"
                  value={connector}
                  onValueChange={setConnector}
                  options={[{ value: "webhook", label: "Webhook (signed delivery)" }].concat(
                    allowBuiltin || connector === "builtin" ? [{ value: "builtin", label: "Built-in code of this server" }] : [],
                  )}
                />
              </Field>
              {connector === "webhook" ? (
                <Field label="Webhook URL" htmlFor="action-url">
                  <Input id="action-url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://helpdesk.example/hooks/mandate" />
                </Field>
              ) : null}
              <Field
                label="Payload schema (JSON Schema)"
                htmlFor="action-schema"
                hint={
                  <>
                    {connector === "webhook" ? "Required for a webhook. " : "Optional. "}Agents see it; proposals and edits are held to it.
                    {schemaTouched && schemaError ? <span className="mt-1 block text-danger-ink">{schemaError}</span> : null}
                  </>
                }
              >
                <Textarea
                  id="action-schema"
                  value={schema}
                  onChange={(e) => setSchema(e.target.value)}
                  onBlur={() => setSchemaTouched(true)}
                  aria-invalid={schemaTouched && !!schemaError}
                  className="font-mono text-[12px]"
                  rows={5}
                  placeholder={'{\n  "type": "object",\n  "required": ["subject"],\n  "properties": { "subject": { "type": "string" } }\n}'}
                />
              </Field>
              <Field label="Department" htmlFor="action-department" hint="A label for finding things. It limits nothing yet.">
                <Input id="action-department" value={department} onChange={(e) => setDepartment(e.target.value)} placeholder="optional" />
              </Field>
            </div>
          </div>
          {existing?.hasSecret && connector === "webhook" ? (
            <label className="flex items-center gap-2 text-[13.5px]">
              <input type="checkbox" checked={rotate} onChange={(e) => setRotate(e.target.checked)} /> Rotate the signing secret
            </label>
          ) : null}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={define.isPending} disabled={!key.trim() || (schemaTouched && !!schemaError)}>
              Save
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ActionRow({ action, isOwner, allowBuiltin, onChanged }: { action: Action; isOwner: boolean; allowBuiltin: boolean; onChanged: (secret: string) => void }) {
  const [confirming, setConfirming] = React.useState(false);
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.actions.removeAction({ key: action.key }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["actions"] });
      toast.success(`${action.key} removed`);
    },
    onError: (error) => toast.error("Could not remove", { description: errorText(error) }),
  });
  return (
    <li className="border-t border-border px-5 py-4 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[13px] text-accent">{action.key}</span>
        <Badge tone={riskTone[action.risk] ?? "neutral"}>{riskLabel[action.risk] ?? action.risk}</Badge>
        <Badge tone="neutral">costs {units(action.costUnits)}</Badge>
        <Badge tone={action.mayExecute ? "success" : "neutral"}>{action.mayExecute ? "can be delegated" : "a person always decides"}</Badge>
        {action.builtinOnly ? <Badge tone="neutral">built in</Badge> : null}
        {action.removedAt ? <Badge tone="neutral">removed</Badge> : null}
        {isOwner && !action.removedAt ? (
          <div className="ml-auto flex gap-1">
            <DefineActionDialog existing={action} allowBuiltin={allowBuiltin} onDone={onChanged} />
            {action.builtinOnly ? null : confirming ? (
              <>
                <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
                <Button variant="danger" size="sm" onClick={() => remove.mutate()} loading={remove.isPending}>
                  Remove now
                </Button>
              </>
            ) : (
              <Button variant="ghost" size="sm" onClick={() => setConfirming(true)}>
                Remove
              </Button>
            )}
          </div>
        ) : null}
      </div>
      {action.description ? <p className="mt-1 text-[13.5px]">{action.description}</p> : null}
      <p className="mt-1 flex items-center gap-1.5 text-[12.5px] text-muted">
        <Webhook className="size-3.5" aria-hidden />
        {action.connector === "webhook" ? `${action.connectorUrl} · ${action.hasSecret ? "signing secret set" : "no secret"}` : "builtin code"}
        {action.department ? ` · ${action.department}` : ""}
      </p>
    </li>
  );
}

export function AgentsPage() {
  const session = useSession();
  const queryClient = useQueryClient();
  const agents = useQuery({ queryKey: ["agents"], queryFn: () => api.agents.listAgents({}) });
  const actions = useQuery({ queryKey: ["actions"], queryFn: () => api.actions.listActions({}) });
  const [secret, setSecret] = React.useState<string | null>(null);
  const [hookSecret, setHookSecret] = React.useState<string | null>(null);
  // The built-in connector is offered only where this server has built-in actions.
  const allowBuiltin = (actions.data?.actions ?? []).some((a) => a.builtinOnly || a.connector === "builtin");
  const onActionChanged = (s: string) => {
    queryClient.invalidateQueries({ queryKey: ["actions"] });
    if (s) setHookSecret(s);
  };
  return (
    <div>
      <PageHeader
        eyebrow={
          <>
            <Bot className="size-3.5" /> Who may propose what
          </>
        }
        title="Agents & actions"
      />
      <div className="grid max-w-6xl gap-8 px-6 py-6 lg:grid-cols-2 lg:px-8">
        <section className="flex flex-col gap-4">
          <SectionLabel>Agents</SectionLabel>
          <p className="text-[14px] text-muted">
            An agent connects over MCP with its token and proposes in its own name. Any MCP client works: Claude Code, an SDK, a cron job.
          </p>
          {secret ? <NewToken secret={secret} onDone={() => setSecret(null)} /> : null}
          {session.isOwner ? (
            <NewAgent onCreated={() => queryClient.invalidateQueries({ queryKey: ["agents"] })} />
          ) : (
            <p className="text-[13.5px] text-muted">You are a reviewer here. Owners register agents and define actions.</p>
          )}
          {agents.isPending ? (
            <Skeleton className="h-24" />
          ) : agents.isError ? (
            <ErrorState error={agents.error} onRetry={() => agents.refetch()} />
          ) : agents.data.agents.length === 0 ? (
            <EmptyState icon={<Bot />} title="No agents yet">
              {session.isOwner ? "Register one above; it gets a token to connect with." : "An owner registers them."}
            </EmptyState>
          ) : (
            agents.data.agents.map((agent) => <AgentCard key={agent.id} agent={agent} isOwner={session.isOwner} onToken={setSecret} />)
          )}
        </section>
        <section className="flex flex-col gap-4">
          <div className="flex items-center justify-between">
            <SectionLabel>Actions</SectionLabel>
            {session.isOwner ? <DefineActionDialog allowBuiltin={allowBuiltin} onDone={onActionChanged} /> : null}
          </div>
          <p className="text-[14px] text-muted">
            What agents may propose. A decision on a webhook action is delivered to its URL, signed with the action's secret.
          </p>
          {hookSecret ? (
            <Card className="border-accent/60 p-5">
              <p className="font-serif text-[18px]">Signing secret</p>
              <p className="mt-1 text-[13.5px] text-muted">Shown once. The receiver verifies every delivery's Mandate-Signature with it.</p>
              <div className="mt-3">
                <Snippet title="Secret" text={hookSecret} />
              </div>
              <div className="mt-4 flex justify-end">
                <Button size="sm" onClick={() => setHookSecret(null)}>
                  I have saved it
                </Button>
              </div>
            </Card>
          ) : null}
          {actions.isPending ? (
            <Skeleton className="h-24" />
          ) : actions.isError ? (
            <ErrorState error={actions.error} onRetry={() => actions.refetch()} />
          ) : actions.data.actions.length === 0 ? (
            <EmptyState icon={<Webhook />} title="No actions yet">
              Define what agents may propose, and where a decision is delivered.
            </EmptyState>
          ) : (
            <Card>
              <ul>
                {actions.data.actions.map((action) => (
                  <ActionRow key={action.key} action={action} isOwner={session.isOwner} allowBuiltin={allowBuiltin} onChanged={onActionChanged} />
                ))}
              </ul>
            </Card>
          )}
        </section>
      </div>
    </div>
  );
}
