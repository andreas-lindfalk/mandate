import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Bot, Check, Copy, KeyRound, Plus, Webhook } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogTrigger } from "@/components/ui/dialog";
import { Badge, Card, EmptyState, ErrorState, Field, Input, SectionLabel, Skeleton, StaleNote, Textarea } from "@/components/ui/primitives";
import { Select } from "@/components/ui/select";
import type { Action } from "@/gen/mandate/v1/actions_pb";
import type { Agent, Token } from "@/gen/mandate/v1/agents_pb";
import { api } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { errorText } from "@/lib/errors";
import { cn, singleClick, useFocusBack } from "@/lib/utils";
import { durationLabel, formatDateTime, formatRelative, prettyJson, riskLabel, riskTone } from "@/lib/format";
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
  // The card shows at the top of the section, often far from the card the
  // token was issued from: focus moves to it, which also scrolls it into
  // view and has a screen reader read it, so nobody issues a second one.
  const heading = React.useRef<HTMLHeadingElement>(null);
  React.useEffect(() => {
    heading.current?.focus({ preventScroll: true });
    heading.current?.scrollIntoView({ block: "start" });
  }, [secret]);
  return (
    <Card className="border-accent/60 p-6">
      <div className="flex items-start gap-3">
        <KeyRound className="mt-0.5 size-5 shrink-0 text-accent" aria-hidden />
        <div className="min-w-0 flex-1">
          <h2 ref={heading} tabIndex={-1} className="font-serif text-[20px] outline-none">
            The token is ready
          </h2>
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

// Revoking an agent leaves its tokens' own revoked_at alone: the agent row is
// what stops them. A token of a revoked agent is therefore never Active,
// whatever the token says by itself.
function tokenState(token: Token, agent: Agent): { label: string; tone: "success" | "neutral" | "warning" } {
  if (token.revokedAt) return { label: "Revoked", tone: "neutral" };
  if (agent.revokedAt) return { label: "Ended with the agent", tone: "neutral" };
  if (token.expiresAt && timestampDate(token.expiresAt) < new Date()) return { label: "Expired", tone: "warning" };
  return { label: "Active", tone: "success" };
}

function AgentCard({ agent, isOwner, onToken }: { agent: Agent; isOwner: boolean; onToken: (secret: string, focusBack: () => void) => void }) {
  const queryClient = useQueryClient();
  const issueButton = React.useRef<HTMLButtonElement>(null);
  const tokens = useQuery({ queryKey: ["tokens", agent.id], queryFn: () => api.agents.listTokens({ agentId: agent.id }) });
  const [days, setDays] = React.useState("90");
  const issue = useMutation({
    mutationFn: () => api.agents.issueToken({ agentId: agent.id, days: Number(days) }),
    gcTime: 0,
    onSuccess: (response) => {
      queryClient.invalidateQueries({ queryKey: ["tokens", agent.id] });
      onToken(response.secret, () => issueButton.current?.focus());
    },
    onError: (error) => toast.error("Could not issue a token", { description: errorText(error) }),
  });
  const [revoking, setRevoking] = React.useState<string | null>(null);
  const tokenTrigger = useFocusBack(revoking);
  const revokeToken = useMutation({
    mutationFn: (id: string) => api.agents.revokeToken({ id }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["tokens", agent.id] }),
    onError: (error) => toast.error("Could not revoke", { description: errorText(error) }),
    onSettled: () => setRevoking(null),
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
  const revokeTrigger = useFocusBack(confirming);
  const [editingLimits, setEditingLimits] = React.useState(false);
  const limitsTrigger = useFocusBack(editingLimits);
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
  const setMode = useMutation({
    mutationFn: (mode: string) => api.agents.setAgentMode({ id: agent.id, mode }),
    onSuccess: (response) => {
      queryClient.invalidateQueries({ queryKey: ["agents"] });
      toast.success(response.agent?.mode === "live" ? `${agent.name} is live` : `${agent.name} is in dry run`);
    },
    onError: (error) => toast.error("Could not change the mode", { description: errorText(error) }),
  });
  const live = !agent.revokedAt;
  const dryRun = agent.mode === "dry_run";
  return (
    <Card className="p-5">
      <div className="flex flex-wrap items-center gap-2">
        <Bot className="size-4 text-accent" aria-hidden />
        <p className="font-medium">{agent.name}</p>
        {agent.department ? <Badge tone="neutral">{agent.department}</Badge> : null}
        {!live ? <Badge tone="neutral">revoked</Badge> : dryRun ? <Badge tone="accent">dry run</Badge> : <Badge tone="success">live</Badge>}
        <span className="ml-auto text-[12px] text-muted">since {formatDateTime(agent.createdAt)}</span>
      </div>
      <p className="mt-1 font-mono text-[11px] text-muted">{agent.id}</p>
      {live ? (
        <div className="mt-2 flex flex-wrap items-center gap-2 text-[12.5px] text-muted">
          <span>
            {dryRun
              ? "In dry run: what it proposes is recorded and never carried out, while it acts with its own credential."
              : "Live: what it proposes is carried out once a person or a mandate allows it."}
          </span>
          {isOwner ? (
            <Button variant="ghost" size="sm" onClick={() => setMode.mutate(dryRun ? "live" : "dry_run")} loading={setMode.isPending}>
              {dryRun ? "Make live" : "Back to dry run"}
            </Button>
          ) : null}
        </div>
      ) : null}
      {editingLimits ? (
        <form
          className="mt-3 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setLimits.mutate();
          }}
        >
          <Field label="Proposals a minute" htmlFor={`per-minute-${agent.id}`} className="w-40">
            <Input id={`per-minute-${agent.id}`} autoFocus type="number" min={1} max={10000} value={perMinute} onChange={(e) => setPerMinute(e.target.value)} />
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
              ref={limitsTrigger()}
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
      {tokens.isPending ? (
        <Skeleton className="mt-3 h-6" />
      ) : !tokens.data ? (
        <ErrorState className="mt-3 py-4" title="Could not load the tokens" error={tokens.error} onRetry={() => tokens.refetch()} />
      ) : (
        <ul className="mt-3 flex flex-col gap-1 text-[13px]">
          {tokens.data.tokens.map((token) => {
            const state = tokenState(token, agent);
            return (
              <li key={token.id} className="flex flex-wrap items-center gap-2">
                <span className="font-mono">…{token.hint}</span>
                <Badge tone={state.tone}>{state.label}</Badge>
                <span className="text-muted">
                  {state.label === "Active" ? `until ${formatDateTime(token.expiresAt)}` : ""} ·{" "}
                  {token.lastUsedAt ? `used ${formatRelative(token.lastUsedAt)}` : "never used"}
                </span>
                {isOwner && state.label === "Active" ? (
                  revoking === token.id ? (
                    // Cancel takes the trigger's place, so the click that armed this never lands on the danger button.
                    <div className="ml-auto flex gap-2">
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={singleClick(() => revokeToken.mutate(token.id))}
                        loading={revokeToken.isPending && revokeToken.variables === token.id}
                      >
                        Revoke now, cutting the agent off
                      </Button>
                      <Button variant="ghost" size="sm" autoFocus onClick={() => setRevoking(null)}>
                        Cancel
                      </Button>
                    </div>
                  ) : (
                    <Button variant="ghost" size="sm" className="ml-auto" ref={tokenTrigger(token.id)} onClick={() => setRevoking(token.id)}>
                      Revoke token
                    </Button>
                  )
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
      {tokens.isError && tokens.data ? <StaleNote error={tokens.error} className="mt-1.5" /> : null}
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
          <Button variant="secondary" ref={issueButton} onClick={() => issue.mutate()} loading={issue.isPending}>
            <KeyRound /> Issue a token
          </Button>
          <div className="ml-auto flex gap-2">
            {confirming ? (
              <>
                {/* Cancel takes the trigger's place, so the click that armed this never lands on the danger button. */}
                <Button variant="danger" size="sm" onClick={singleClick(() => revokeAgent.mutate())} loading={revokeAgent.isPending}>
                  Revoke the agent
                </Button>
                <Button variant="ghost" size="sm" autoFocus onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button variant="ghost" size="sm" ref={revokeTrigger()} onClick={() => setConfirming(true)}>
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
  const [live, setLive] = React.useState(false);
  const create = useMutation({
    mutationFn: () => api.agents.createAgent({ name: name.trim(), department: department.trim(), mode: live ? "live" : "dry_run" }),
    onSuccess: () => {
      setName("");
      setDepartment("");
      setLive(false);
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
        <label className="flex items-start gap-2 text-[13px] sm:col-span-3">
          <input type="checkbox" className="mt-0.5" checked={live} onChange={(e) => setLive(e.target.checked)} />
          <span>
            Live from the start. Without it the agent starts in dry run: it keeps its own credential, and pgmandate only records what it proposes,
            until you make it live.
          </span>
        </label>
      </form>
    </Card>
  );
}

function DefineActionDialog({ existing, allowBuiltin, onDone }: { existing?: Action; allowBuiltin: boolean; onDone: (defined?: Action, before?: Action) => void }) {
  const [open, setOpen] = React.useState(false);
  const [key, setKey] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [risk, setRisk] = React.useState("low");
  const [waitHours, setWaitHours] = React.useState("168");
  const [deliverMinutes, setDeliverMinutes] = React.useState("60");
  const [mayExecute, setMayExecute] = React.useState(false);
  const [connector, setConnector] = React.useState("webhook");
  const [url, setUrl] = React.useState("");
  const [schema, setSchema] = React.useState("");
  const [department, setDepartment] = React.useState("");
  // The schema's error shows once the field has been left or a save tried,
  // not on a dialog that has only just opened.
  const [schemaTouched, setSchemaTouched] = React.useState(false);
  // Every opening starts from the action as it is saved, so edits that
  // were cancelled never come back to be saved by accident.
  const reset = () => {
    setKey(existing?.key ?? "");
    setDescription(existing?.description ?? "");
    setRisk(existing?.risk ?? "low");
    setWaitHours(String(existing?.waitSeconds ? Number(existing.waitSeconds) / 3600 : 168));
    setDeliverMinutes(String(existing?.deliverSeconds ? Number(existing.deliverSeconds) / 60 : 60));
    setMayExecute(existing?.mayExecute ?? false);
    setConnector(existing?.connector ?? "webhook");
    setUrl(existing?.connectorUrl ?? "");
    setSchema(existing?.schemaJson ? prettyJson(existing.schemaJson) : "");
    setDepartment(existing?.department ?? "");
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
  // A time of 0 seconds, as an empty field would give, reads to the server as
  // "not given" and is replaced by its default, so a save waits for both times.
  const seconds = (value: string, unit: number) => (value.trim() === "" ? NaN : Math.round(Number(value) * unit));
  const waitSeconds = seconds(waitHours, 3600);
  const deliverSeconds = seconds(deliverMinutes, 60);
  const timesProblem = !(Number.isSafeInteger(waitSeconds) && waitSeconds > 0) || !(Number.isSafeInteger(deliverSeconds) && deliverSeconds > 0) ? "Give how long it waits and how soon it is carried out." : "";
  const define = useMutation({
    mutationFn: () =>
      api.actions.defineAction({
        key: key.trim(),
        description,
        risk,
        waitSeconds: BigInt(waitSeconds),
        deliverSeconds: BigInt(deliverSeconds),
        mayExecute: mayExecute && !highWebhook,
        connector,
        connectorUrl: connector === "webhook" ? url.trim() : "",
        schemaJson: schema.trim(),
        department,
      }),
    onSuccess: (response) => {
      toast.success(existing ? "Action updated" : "Action defined");
      setOpen(false);
      onDone(response.action, existing);
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
            if (schemaError || timesProblem) return;
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
                <Field label="Waits up to, hours" htmlFor="action-wait" hint="Then the proposal expires. 1 minute to 720 hours.">
                  <Input id="action-wait" type="number" required min={1 / 60} max={720} step="any" value={waitHours} onChange={(e) => setWaitHours(e.target.value)} />
                </Field>
                <Field label="Carried out within, minutes" htmlFor="action-deliver" hint="After a decision. 2 to 10080.">
                  <Input id="action-deliver" type="number" required min={2} max={10080} step="any" value={deliverMinutes} onChange={(e) => setDeliverMinutes(e.target.value)} />
                </Field>
              </div>
              {timesProblem ? (
                <p id="action-times-problem" className="-mt-2 text-[12px] text-danger-ink">
                  {timesProblem}
                </p>
              ) : null}
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
          {existing?.connector === "webhook" && connector === "webhook" && url.trim() !== existing.connectorUrl ? (
            <p className="text-[13px] text-muted">
              A new URL gives the action a new audience: the receiver at the old one accepts nothing more, and the new one must be told the new audience.
            </p>
          ) : null}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              type="submit"
              loading={define.isPending}
              disabled={!key.trim() || !!timesProblem || (schemaTouched && !!schemaError)}
              aria-describedby={timesProblem ? "action-times-problem" : undefined}
            >
              Save
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function ActionRow({ action, isOwner, allowBuiltin, onChanged }: { action: Action; isOwner: boolean; allowBuiltin: boolean; onChanged: (defined?: Action, before?: Action) => void }) {
  const [confirming, setConfirming] = React.useState(false);
  const removeTrigger = useFocusBack(confirming);
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
        <Badge tone="neutral">waits up to {durationLabel(action.waitSeconds)}</Badge>
        <Badge tone={action.mayExecute ? "success" : "neutral"}>{action.mayExecute ? "can be delegated" : "a person always decides"}</Badge>
        {action.builtinOnly ? <Badge tone="neutral">built in</Badge> : null}
        {action.removedAt ? <Badge tone="neutral">removed</Badge> : null}
        {isOwner && !action.removedAt ? (
          <div className="ml-auto flex gap-1">
            <DefineActionDialog existing={action} allowBuiltin={allowBuiltin} onDone={onChanged} />
            {action.builtinOnly ? null : confirming ? (
              <>
                {/* Cancel takes the trigger's place, so the click that armed this never lands on the danger button. */}
                <Button variant="danger" size="sm" onClick={singleClick(() => remove.mutate())} loading={remove.isPending}>
                  Remove now
                </Button>
                <Button variant="ghost" size="sm" autoFocus onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button variant="ghost" size="sm" ref={removeTrigger()} onClick={() => setConfirming(true)}>
                Remove
              </Button>
            )}
          </div>
        ) : null}
      </div>
      {action.description ? <p className="mt-1 text-[13.5px]">{action.description}</p> : null}
      <p className="mt-1 flex items-center gap-1.5 text-[12.5px] text-muted">
        <Webhook className="size-3.5" aria-hidden />
        {action.connector === "webhook" ? action.connectorUrl : "builtin code"}
        {action.department ? ` · ${action.department}` : ""}
      </p>
      {action.audience ? (
        <p className="mt-1 break-all font-mono text-[12px] text-muted" title="The audience its receiver accepts">
          {action.audience}
        </p>
      ) : null}
    </li>
  );
}

export function AgentsPage() {
  // Keyed on the tenant, so a token shown once, or a receiver's details,
  // never stay on screen for the next tenant.
  const tenantId = useSession().tenant?.tenantId;
  return <TenantAgentsPage key={tenantId} />;
}

function TenantAgentsPage() {
  const session = useSession();
  const queryClient = useQueryClient();
  const agents = useQuery({ queryKey: ["agents"], queryFn: () => api.agents.listAgents({}) });
  const actions = useQuery({ queryKey: ["actions"], queryFn: () => api.actions.listActions({}) });
  const [secret, setSecret] = React.useState<string | null>(null);
  // Where focus goes once the token card is closed: the Issue button it came from.
  const tokenFocusBack = React.useRef<() => void>(undefined);
  // A webhook defined, or moved to a new audience: its receiver must be told.
  const [receiverFor, setReceiverFor] = React.useState<Action | null>(null);
  // The built-in connector is offered only where this server has built-in actions.
  const allowBuiltin = (actions.data?.actions ?? []).some((a) => a.builtinOnly || a.connector === "builtin");
  const onActionChanged = (defined?: Action, before?: Action) => {
    queryClient.invalidateQueries({ queryKey: ["actions"] });
    if (defined?.audience && defined.audience !== before?.audience) setReceiverFor(defined);
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
          {secret ? (
            <NewToken
              secret={secret}
              onDone={() => {
                tokenFocusBack.current?.();
                setSecret(null);
              }}
            />
          ) : null}
          {session.isOwner ? (
            <NewAgent onCreated={() => queryClient.invalidateQueries({ queryKey: ["agents"] })} />
          ) : (
            <p className="text-[13.5px] text-muted">You are a reviewer here. Owners register agents and define actions.</p>
          )}
          {/* A failed refetch keeps the data it had, so a card's open edit
              or armed revoke survives; only a first load that fails shows
              the error. */}
          {agents.isError && agents.data ? <StaleNote error={agents.error} /> : null}
          {agents.isPending ? (
            <Skeleton className="h-24" />
          ) : !agents.data ? (
            <ErrorState error={agents.error} onRetry={() => agents.refetch()} />
          ) : agents.data.agents.length === 0 ? (
            <EmptyState icon={<Bot />} title="No agents yet">
              {session.isOwner ? "Register one above; it gets a token to connect with." : "An owner registers them."}
            </EmptyState>
          ) : (
            agents.data.agents.map((agent) => (
              <AgentCard
                key={agent.id}
                agent={agent}
                isOwner={session.isOwner}
                onToken={(issued, focusBack) => {
                  tokenFocusBack.current = focusBack;
                  setSecret(issued);
                }}
              />
            ))
          )}
        </section>
        <section className="flex flex-col gap-4">
          <div className="flex items-center justify-between">
            <SectionLabel>Actions</SectionLabel>
            {session.isOwner ? <DefineActionDialog allowBuiltin={allowBuiltin} onDone={onActionChanged} /> : null}
          </div>
          <p className="text-[14px] text-muted">
            What agents may propose. A decision on a webhook action is delivered to its URL, signed with this server's key and addressed to the
            action's audience.
          </p>
          {receiverFor ? (
            <Card className="border-accent/60 p-5">
              <p className="font-serif text-[18px]">Tell the receiver</p>
              <p className="mt-1 text-[13.5px] text-muted">
                The receiver at {receiverFor.connectorUrl} accepts only deliveries for this tenant and audience, and verifies each one with the published
                keys. None of them is a secret.
              </p>
              <div className="mt-3 flex flex-col gap-3">
                {session.tenant ? <Snippet title="Tenant" text={session.tenant.tenantId} /> : null}
                <Snippet title="Audience" text={receiverFor.audience} />
                <Snippet title="Keys" text={`${window.location.origin}/.well-known/pgmandate-keys`} />
              </div>
              <div className="mt-4 flex justify-end">
                <Button size="sm" onClick={() => setReceiverFor(null)}>
                  Done
                </Button>
              </div>
            </Card>
          ) : null}
          {actions.isError && actions.data ? <StaleNote error={actions.error} /> : null}
          {actions.isPending ? (
            <Skeleton className="h-24" />
          ) : !actions.data ? (
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
