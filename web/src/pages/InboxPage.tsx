import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Bot, Check, CircleCheck, CircleDashed, Inbox, RotateCw, ShieldAlert, X } from "lucide-react";
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Badge, Card, EmptyState, ErrorState, Field, Skeleton, Textarea } from "@/components/ui/primitives";
import type { Event } from "@/gen/mandate/v1/evidence_pb";
import type { Delivery, Proposal } from "@/gen/mandate/v1/proposals_pb";
import { api } from "@/lib/api";
import { errorText } from "@/lib/errors";
import {
  actorLabel,
  eventLabel,
  flagLabel,
  formatDateTime,
  formatRelative,
  hostOf,
  prettyJson,
  riskLabel,
  riskTone,
  shortId,
  skipLabel,
  statusLabel,
  units,
} from "@/lib/format";
import { useSession } from "@/lib/session";
import { cn } from "@/lib/utils";

const tabs = [
  { key: "pending", label: "Waiting" },
  { key: "executed", label: "Allowed by mandate" },
  { key: "approved", label: "Approved" },
  { key: "rejected", label: "Rejected" },
] as const;

function RiskBadge({ risk }: { risk: string }) {
  return <Badge tone={riskTone[risk] ?? "neutral"}>{riskLabel[risk] ?? risk} risk</Badge>;
}

function DeliveryBadge({ delivery }: { delivery?: Delivery }) {
  if (!delivery) return null;
  if (delivery.status === "failed") return <Badge tone="danger">not delivered</Badge>;
  if (delivery.status === "pending") return <Badge tone="neutral">delivering</Badge>;
  return null;
}

/** The order of setting a tenant up, shown to an owner until the first two steps are done. */
function SetupCard() {
  const actions = useQuery({ queryKey: ["actions"], queryFn: () => api.actions.listActions({}) });
  const agents = useQuery({ queryKey: ["agents"], queryFn: () => api.agents.listAgents({}) });
  const mandates = useQuery({ queryKey: ["mandates", false], queryFn: () => api.mandates.listMandates({}) });
  if (!actions.data || !agents.data || !mandates.data) return null;
  const hasAction = actions.data.actions.some((a) => !a.builtinOnly);
  const hasAgent = agents.data.agents.some((a) => !a.revokedAt);
  const hasMandate = mandates.data.mandates.length > 0;
  if (hasAction && hasAgent) return null;
  const steps = [
    { done: hasAction, to: "/agents", title: "Define an action", text: "What an agent may ask for, such as tickets.create, and where a decision is sent." },
    { done: hasAgent, to: "/agents", title: "Register an agent and connect it", text: "It gets a token; paste the ready-made line into Claude Code or any MCP client." },
    { done: hasMandate, to: "/mandates", title: "Grant a mandate (optional)", text: "Until you do, every proposal waits here for a person." },
  ];
  return (
    <Card className="mb-6 p-6">
      <p className="label-mono text-accent">Set up</p>
      <h2 className="mt-1 font-serif text-2xl">Three steps to the first proposal</h2>
      <ol className="mt-4 flex flex-col gap-3">
        {steps.map((step, i) => (
          <li key={step.title} className="flex gap-3">
            {step.done ? <CircleCheck className="mt-0.5 size-5 shrink-0 text-success" aria-label="Done" /> : <CircleDashed className="mt-0.5 size-5 shrink-0 text-muted" aria-label="To do" />}
            <div>
              <Link to={step.to} className="font-medium underline-offset-4 hover:underline">
                {i + 1}. {step.title}
              </Link>
              <p className="text-[13.5px] text-muted">{step.text}</p>
            </div>
          </li>
        ))}
      </ol>
    </Card>
  );
}

function Row({ proposal, description, selected, onSelect }: { proposal: Proposal; description?: string; selected: boolean; onSelect: () => void }) {
  return (
    <li>
      <button
        type="button"
        onClick={onSelect}
        aria-current={selected || undefined}
        className={cn("flex w-full flex-col gap-1.5 border-t border-border px-5 py-4 text-left transition-colors hover:bg-row-hover", selected && "bg-selected")}
      >
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[12px] text-accent">{proposal.action}</span>
          <RiskBadge risk={proposal.risk} />
          {proposal.flags.length ? (
            <Badge tone="danger">
              <ShieldAlert /> flagged
            </Badge>
          ) : null}
          <DeliveryBadge delivery={proposal.delivery} />
          <span className="ml-auto text-[12px] text-muted">{formatRelative(proposal.createdAt)}</span>
        </div>
        {/* What the action does leads; the agent's own words follow, marked as theirs. */}
        <p className="font-medium">{description || proposal.action}</p>
        <p className="text-[13px]">
          <span className="text-muted">Agent's summary: </span>
          {proposal.title}
        </p>
        <p className="text-[12.5px] text-muted">
          <Bot className="mr-1 inline size-3.5" aria-hidden />
          {proposal.agentName || shortId(proposal.agentId)}
          {proposal.status === "pending" && proposal.skipReason ? ` · ${skipLabel[proposal.skipReason] ?? proposal.skipReason}` : ""}
        </p>
      </button>
    </li>
  );
}

function History({ events }: { events: Event[] }) {
  return (
    <ol className="flex flex-col gap-2">
      {events.map((event) => (
        <li key={event.seq} className="flex gap-3 text-[13px]">
          <span className="w-12 shrink-0 font-mono text-[12px] text-muted">#{Number(event.seq)}</span>
          <div className="min-w-0 flex-1">
            <p>
              <span className="font-medium" title={event.type}>
                {eventLabel[event.type] ?? event.type}
              </span>
              <span className="text-muted">
                {" "}
                · {actorLabel[event.actorKind] ?? event.actorKind}
                {event.actorName ? ` ${event.actorName}` : ""} · {formatDateTime(event.createdAt)}
              </span>
            </p>
            <details className="mt-0.5">
              <summary className="cursor-pointer text-[12px] text-muted">Details</summary>
              <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-all font-mono text-[12px] text-muted">{prettyJson(event.payloadJson)}</pre>
            </details>
          </div>
        </li>
      ))}
    </ol>
  );
}

function DeliveryCard({ proposalId, delivery, onChanged }: { proposalId: string; delivery: Delivery; onChanged: () => void }) {
  const redeliver = useMutation({
    mutationFn: () => api.proposals.redeliverProposal({ id: proposalId }),
    onSuccess: () => {
      toast.success("Sent again");
      onChanged();
    },
    onError: (error) => toast.error("Could not send it again", { description: errorText(error) }),
  });
  const host = hostOf(delivery.url);
  if (delivery.status === "failed") {
    return (
      <Card className="border-danger/30 bg-danger-soft p-4 text-[13.5px] text-danger-ink">
        <p className="font-medium">Decided, but not delivered.</p>
        <p className="mt-1">
          {host} did not accept it after {delivery.attempts} {delivery.attempts === 1 ? "attempt" : "attempts"}
          {delivery.lastError ? `: ${delivery.lastError}` : ""}. The decision stands; the receiving system has not acted on it.
        </p>
        <Button variant="secondary" size="sm" className="mt-3" onClick={() => redeliver.mutate()} loading={redeliver.isPending}>
          <RotateCw /> Send again
        </Button>
      </Card>
    );
  }
  if (delivery.status === "pending") {
    return (
      <Card className="bg-sunken p-4 text-[13.5px]">
        Being delivered to {host}
        {delivery.attempts ? ` (${delivery.attempts} of ${delivery.maxAttempts} attempts so far${delivery.lastError ? `: ${delivery.lastError}` : ""})` : ""}.
      </Card>
    );
  }
  return (
    <Card className="bg-sunken p-4 text-[13.5px]">
      <p>
        Delivered to {host} {formatDateTime(delivery.deliveredAt)}.
      </p>
      {delivery.resultJson ? (
        <pre className="mt-2 overflow-x-auto font-mono text-[12px] text-muted" aria-label="What the receiving system answered">
          {prettyJson(delivery.resultJson)}
        </pre>
      ) : null}
    </Card>
  );
}

function sameJson(a: string, b: string) {
  try {
    return JSON.stringify(JSON.parse(a)) === JSON.stringify(JSON.parse(b));
  } catch {
    return false;
  }
}

function Detail({ id, onDecided }: { id: string; onDecided: () => void }) {
  const queryClient = useQueryClient();
  const one = useQuery({ queryKey: ["proposal", id], queryFn: () => api.proposals.getProposal({ id }), refetchInterval: 5_000 });
  const actions = useQuery({ queryKey: ["actions", "all"], queryFn: () => api.actions.listActions({ includeRemoved: true }) });
  const [note, setNote] = React.useState("");
  // The server asks for a confirmation the page did not foresee, such as
  // an edit its own assessment makes high risk: then the page asks too.
  const [confirmAsked, setConfirmAsked] = React.useState(false);
  const [editing, setEditing] = React.useState(false);
  const [payload, setPayload] = React.useState("");
  const [confirm, setConfirm] = React.useState(false);
  // After a decision the next proposal opens in place of this one; focus
  // moves to its heading instead of being lost with the old buttons.
  const heading = React.useRef<HTMLHeadingElement>(null);
  const loaded = !!one.data;
  React.useEffect(() => {
    if (loaded && (document.activeElement === document.body || document.activeElement === null)) heading.current?.focus();
  }, [loaded]);
  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["proposals"] });
    queryClient.invalidateQueries({ queryKey: ["proposal", id] });
    queryClient.invalidateQueries({ queryKey: ["chain"] });
  };
  const original = one.data?.proposal?.payloadJson ?? "";
  let payloadError = "";
  if (editing) {
    try {
      JSON.parse(payload);
    } catch {
      payloadError = "This is not valid JSON.";
    }
  }
  const changed = editing && !payloadError && !sameJson(payload, original);
  const approve = useMutation({
    // The payload is sent only when it really differs, so looking at it
    // never marks a proposal as edited.
    mutationFn: () => api.proposals.approveProposal({ id, note, confirm, payloadJson: changed ? payload : "" }),
    onSuccess: () => {
      toast.success(changed ? "Approved with your edits" : "Approved");
      invalidate();
      onDecided();
    },
    onError: (error) => {
      if (errorText(error).includes("confirmed explicitly")) setConfirmAsked(true);
      toast.error("Could not approve", { description: errorText(error) });
    },
  });
  const reject = useMutation({
    mutationFn: () => api.proposals.rejectProposal({ id, note }),
    onSuccess: () => {
      toast.success("Rejected");
      invalidate();
      onDecided();
    },
    onError: (error) => toast.error("Could not reject", { description: errorText(error) }),
  });

  if (one.isPending) return <Skeleton className="h-64" />;
  if (one.isError) return <ErrorState error={one.error} onRetry={() => one.refetch()} />;
  const p = one.data.proposal!;
  const pending = p.status === "pending";
  const action = actions.data?.actions.find((a) => a.key === p.action);
  // The server says what risk an approval would run at, under the action
  // as it is now; empty when the action is gone and it can only be rejected.
  const approvalRisk = p.approvalRisk || p.risk;
  const raisedToHigh = p.risk !== "high" && approvalRisk === "high";
  const needsConfirm = approvalRisk === "high" || confirmAsked;
  const actionGone = pending && !p.approvalRisk;
  const decision = one.data.history.find((e) => e.type === "proposal.approved" || e.type === "proposal.rejected");
  return (
    <div className="flex flex-col gap-5">
      <Link to="/inbox" className="flex items-center gap-1 text-[13px] text-muted hover:text-foreground lg:hidden">
        <ArrowLeft className="size-3.5" /> Back to the inbox
      </Link>
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[12px] text-accent">{p.action}</span>
          <RiskBadge risk={p.risk} />
          <Badge tone={p.status === "pending" ? "warning" : p.status === "rejected" ? "neutral" : "success"}>{statusLabel[p.status] ?? p.status}</Badge>
          {p.flags.map((flag) => (
            <Badge key={flag} tone="danger">
              {flagLabel[flag] ?? flag}
            </Badge>
          ))}
          {p.edited ? <Badge tone="accent">edited by the approver</Badge> : null}
        </div>
        <h2 ref={heading} tabIndex={-1} className="mt-3 font-serif text-[26px] leading-tight outline-none">
          {action?.description || p.action}
        </h2>
        <p className="mt-2 text-[14px]">
          <span className="label-mono mr-2 text-[11px] text-muted">Agent's summary</span>
          {p.title}
        </p>
        <p className="mt-1 text-[13px] text-muted">
          Proposed by {p.agentName || shortId(p.agentId)} · {formatDateTime(p.createdAt)} · costs {units(p.costUnits)}
          {p.runId ? (
            <span title="The agent's own id for the task this proposal belongs to">{` · task ${p.runId}`}</span>
          ) : null}
        </p>
      </div>

      {p.delivery ? <DeliveryCard proposalId={p.id} delivery={p.delivery} onChanged={invalidate} /> : null}
      {pending && p.skipReason ? (
        <Card className="bg-warning-soft p-4 text-[13.5px] text-warning-ink">Waits for a person: {skipLabel[p.skipReason] ?? p.skipReason}.</Card>
      ) : null}
      {p.executionBasis ? (
        <Card className="bg-success-soft p-4 text-[13.5px] text-success">
          Allowed without a person under{" "}
          <Link to="/mandates" className="underline underline-offset-2">
            mandate {shortId(p.executionBasis.mandateId)}
          </Link>{" "}
          ({p.executionBasis.policyRef}), up to {riskLabel[p.executionBasis.maxRisk]?.toLowerCase()} risk. {Number(p.executionBasis.spentUnits)} of{" "}
          {Number(p.executionBasis.budgetUnits)} units of this month's budget were used before this one.
        </Card>
      ) : null}
      {decision ? (
        <Card className="bg-sunken p-4 text-[13.5px]">
          {statusLabel[p.status]} by {decision.actorName || "a person"}, {formatDateTime(p.decidedAt)}
          {p.decisionNote ? `: “${p.decisionNote}”` : "."}
        </Card>
      ) : null}

      <div>
        <p className="label-mono mb-2 text-[12px] text-muted">Where it goes</p>
        <p className="text-[14px]">
          {actionGone
            ? "The action has been removed, so this can only be rejected."
            : action?.connector === "webhook" && action.connectorUrl
              ? `An approval is sent to ${hostOf(action.connectorUrl)}, which carries it out.`
              : "An approval is carried out by this server's own code."}
        </p>
      </div>

      <div>
        <div className="mb-2 flex items-center justify-between">
          <p className="label-mono text-[12px] text-muted">Payload</p>
          {pending ? (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                if (!editing) setPayload(prettyJson(original));
                setEditing(!editing);
              }}
            >
              {editing ? "Discard edits" : "Edit before approving"}
            </Button>
          ) : null}
        </div>
        {editing ? (
          <>
            <Textarea
              value={payload}
              onChange={(e) => setPayload(e.target.value)}
              className="font-mono text-[12px]"
              rows={9}
              aria-label="Payload, edited before approval"
              aria-invalid={!!payloadError}
            />
            <p className={cn("mt-1 text-[12px]", payloadError ? "text-danger-ink" : "text-muted")}>
              {payloadError || "Your edit is checked against the action's schema and recorded as an edit."}
            </p>
          </>
        ) : (
          <pre className="overflow-x-auto rounded-[var(--radius-control)] bg-espresso px-4 py-3 font-mono text-[12px] leading-relaxed text-cream">
            {prettyJson(p.payloadJson)}
          </pre>
        )}
        {p.edited && p.proposedPayloadJson ? (
          <div className="mt-3">
            <p className="label-mono mb-2 text-[12px] text-muted">As the agent proposed it, before the edit</p>
            <pre className="overflow-x-auto rounded-[var(--radius-control)] bg-sunken px-4 py-3 font-mono text-[12px] leading-relaxed">
              {prettyJson(p.proposedPayloadJson)}
            </pre>
          </div>
        ) : null}
      </div>

      {pending ? (
        <div className="flex flex-col gap-3 border-t border-border pt-5">
          <Field label="Note" htmlFor="note" hint="Kept in the audit trail with the decision. Required to reject: the agent and the auditor will read it.">
            <Textarea id="note" value={note} onChange={(e) => setNote(e.target.value)} rows={2} />
          </Field>
          {needsConfirm ? (
            <label className="flex items-center gap-2 text-[13.5px]">
              <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />
              {raisedToHigh
                ? "The action has been made high risk since this was proposed. I confirm it."
                : confirmAsked && approvalRisk !== "high"
                  ? "Your edit makes this high risk. I confirm it."
                  : "I confirm this high risk action"}
            </label>
          ) : null}
          <div className="flex gap-2">
            {actionGone ? null : (
              <Button variant="success" onClick={() => approve.mutate()} loading={approve.isPending} disabled={(needsConfirm && !confirm) || !!payloadError}>
                <Check /> {changed ? "Approve with edits" : "Approve"}
              </Button>
            )}
            <Button variant="danger" className="ml-auto" onClick={() => reject.mutate()} loading={reject.isPending} disabled={!note.trim()}>
              <X /> Reject
            </Button>
          </div>
        </div>
      ) : null}

      <div>
        <p className="label-mono mb-2 text-[12px] text-muted">History</p>
        <History events={one.data.history} />
      </div>
    </div>
  );
}

export function InboxPage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const session = useSession();
  const [tab, setTab] = React.useState<(typeof tabs)[number]["key"]>("pending");
  const list = useQuery({
    queryKey: ["proposals", tab],
    // What waits is a queue, worked oldest first; the rest reads newest first.
    queryFn: () => api.proposals.listProposals({ status: tab, limit: 200, oldestFirst: tab === "pending" }),
    refetchInterval: 10_000,
  });
  const proposals = list.data?.proposals ?? [];
  const total = Number(list.data?.total ?? 0);
  const allActions = useQuery({ queryKey: ["actions", "all"], queryFn: () => api.actions.listActions({ includeRemoved: true }) });
  const descriptions = new Map((allActions.data?.actions ?? []).map((a) => [a.key, a.description]));
  // The panel takes focus when the last waiting proposal is decided, so a
  // keyboard user is not left on the page itself.
  const focusPanel = () => window.setTimeout(() => document.getElementById("inbox-panel")?.focus(), 0);
  const chooseTab = (key: (typeof tabs)[number]["key"]) => {
    setTab(key);
    // An open proposal hides the list on a narrow screen: close it, so the
    // tab shows what it says.
    if (id) navigate("/inbox");
  };
  // After a decision the next waiting proposal opens, so a reviewer can
  // work through the queue without going back to the list.
  const next = () => {
    const at = proposals.findIndex((p) => p.id === id);
    const after = [...proposals.slice(at + 1), ...proposals.slice(0, Math.max(at, 0))].filter((p) => p.id !== id && p.status === "pending");
    if (tab === "pending" && after.length) {
      navigate(`/inbox/${after[0].id}`);
    } else {
      navigate("/inbox");
      focusPanel();
    }
  };
  return (
    <div>
      <PageHeader
        eyebrow={
          <>
            <Inbox className="size-3.5" /> Proposals
          </>
        }
        title="Inbox"
      />
      <div role="tablist" aria-label="Proposals by state" className="flex flex-wrap gap-1 border-b border-border px-6 lg:px-8">
        {tabs.map((t, i) => (
          <button
            key={t.key}
            id={`tab-${t.key}`}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            aria-controls="inbox-panel"
            tabIndex={tab === t.key ? 0 : -1}
            onClick={() => chooseTab(t.key)}
            onKeyDown={(e) => {
              // Arrow keys move between tabs, as for any tab list.
              if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
              e.preventDefault();
              const to = tabs[(i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
              chooseTab(to.key);
              document.getElementById(`tab-${to.key}`)?.focus();
            }}
            className={cn("-mb-px border-b-2 px-3 py-3 text-[14px]", tab === t.key ? "border-accent font-medium" : "border-transparent text-muted hover:text-foreground")}
          >
            {t.label}
            {t.key === "pending" && tab === "pending" && total ? ` (${total})` : ""}
          </button>
        ))}
      </div>
      <div id="inbox-panel" role="tabpanel" tabIndex={-1} aria-labelledby={`tab-${tab}`} className="px-6 py-6 outline-none lg:px-8">
        {session.isOwner ? <SetupCard /> : null}
        <div className="grid gap-6 lg:grid-cols-[minmax(320px,400px)_1fr]">
          <div className={cn(id && "hidden lg:block")}>
            {list.isPending ? (
              <Skeleton className="h-40" />
            ) : list.isError ? (
              <ErrorState error={list.error} onRetry={() => list.refetch()} />
            ) : proposals.length === 0 ? (
              <EmptyState icon={<Inbox />} title={tab === "pending" ? "Nothing waits for you" : "Nothing here yet"}>
                {tab === "pending" ? "When an agent proposes something no mandate covers, it appears here for a person to decide." : null}
              </EmptyState>
            ) : (
              <Card>
                <ul className="[&>li:first-child>button]:border-t-0">
                  {proposals.map((p) => (
                    <Row key={p.id} proposal={p} description={descriptions.get(p.action)} selected={p.id === id} onSelect={() => navigate(`/inbox/${p.id}`)} />
                  ))}
                </ul>
              </Card>
            )}
            {total > proposals.length ? (
              <p className="mt-2 text-[12px] text-muted">
                Showing {proposals.length} of {total}, {tab === "pending" ? "oldest" : "newest"} first.
              </p>
            ) : null}
          </div>
          <div>{id ? <Detail key={id} id={id} onDecided={next} /> : <p className="hidden pt-10 text-center text-sm text-muted lg:block">Select a proposal.</p>}</div>
        </div>
      </div>
    </div>
  );
}
