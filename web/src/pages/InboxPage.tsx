import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Bot, Check, CircleCheck, CircleDashed, Inbox, RotateCw, ShieldAlert, X } from "lucide-react";
import * as React from "react";
import { Link, useNavigate, useParams } from "react-router";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Badge, Card, EmptyState, ErrorState, Field, Skeleton, StaleNote, Textarea } from "@/components/ui/primitives";
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
  skipText,
  statusLabel,
  executions,
  toDate,
} from "@/lib/format";
import { sameJson } from "@/lib/json";
import { useSession } from "@/lib/session";
import { cn } from "@/lib/utils";

// Waiting dry runs have a tab of their own: nobody decides them, so they
// stay out of the queue a person works and out of its count.
const tabs: readonly { key: string; label: string; status: string; dryRun?: boolean }[] = [
  { key: "pending", label: "Waiting", status: "pending", dryRun: false },
  { key: "executed", label: "Allowed by mandate", status: "executed" },
  { key: "approved", label: "Approved", status: "approved" },
  { key: "rejected", label: "Rejected", status: "rejected" },
  { key: "withdrawn", label: "Withdrawn", status: "withdrawn" },
  { key: "expired", label: "Expired", status: "expired" },
  { key: "dry_run", label: "Dry runs", status: "pending", dryRun: true },
  { key: "would_allow", label: "Dry runs allowed", status: "would_allow" },
  { key: "dismissed", label: "Dry runs dismissed", status: "dismissed" },
];

function RiskBadge({ risk }: { risk: string }) {
  return <Badge tone={riskTone[risk] ?? "neutral"}>{riskLabel[risk] ?? risk} risk</Badge>;
}

function DeliveryBadge({ delivery }: { delivery?: Delivery }) {
  if (!delivery) return null;
  switch (delivery.status) {
    case "pending":
      return <Badge tone="neutral">delivering</Badge>;
    case "refused":
      return <Badge tone="danger">refused</Badge>;
    case "not_sent":
      return <Badge tone="danger">not delivered</Badge>;
    case "unknown":
      return <Badge tone="warning">outcome unknown</Badge>;
    case "settled_not_done":
      return <Badge tone="danger">not carried out</Badge>;
    default:
      return null;
  }
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
        <span className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[12px] text-accent">{proposal.action}</span>
          <RiskBadge risk={proposal.risk} />
          {proposal.dryRun ? <Badge tone="accent">dry run</Badge> : null}
          {proposal.flags.length ? (
            <Badge tone="danger">
              <ShieldAlert /> flagged
            </Badge>
          ) : null}
          <DeliveryBadge delivery={proposal.delivery} />
          <span className="ml-auto text-[12px] text-muted">{formatRelative(proposal.createdAt)}</span>
        </span>
        {/* What the action does leads; the agent's own words follow, marked as theirs. */}
        <span className="block font-medium">{description || proposal.action}</span>
        <span className="block text-[13px]">
          <span className="text-muted">Agent's summary: </span>
          {proposal.title}
        </span>
        <span className="block text-[12.5px] text-muted">
          <Bot className="mr-1 inline size-3.5" aria-hidden />
          {proposal.agentName || shortId(proposal.agentId)}
          {proposal.status === "pending" && proposal.skipReason ? ` · ${skipText(proposal.skipReason)}` : ""}
        </span>
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
  // The clock moves on while the card is open: sending again ends, and
  // settling opens, at a moment the card must notice.
  const [now, setNow] = React.useState(() => new Date());
  React.useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 15_000);
    return () => clearInterval(timer);
  }, []);
  const sendAgain = useMutation({
    mutationFn: () => api.proposals.redeliverProposal({ id: proposalId }),
    onSuccess: () => {
      toast.success("Sent again");
      onChanged();
    },
    onError: (error) => toast.error("Could not send it again", { description: errorText(error) }),
  });
  const settle = useMutation({
    mutationFn: (done: boolean) => api.proposals.settleDelivery({ id: proposalId, done }),
    onSuccess: () => {
      toast.success("Settled");
      onChanged();
    },
    onError: (error) => toast.error("Could not settle it", { description: errorText(error) }),
  });
  const host = hostOf(delivery.url);
  const attempts = `${delivery.attempts} ${delivery.attempts === 1 ? "attempt" : "attempts"}`;
  const answer = delivery.resultJson ? (
    <pre className="mt-2 overflow-x-auto font-mono text-[12px]" aria-label="What the receiving system answered">
      {prettyJson(delivery.resultJson)}
    </pre>
  ) : null;
  switch (delivery.status) {
    case "pending":
      return (
        <Card className="bg-sunken p-4 text-[13.5px]">
          Being delivered to {host}
          {delivery.attempts ? ` (${delivery.attempts} of ${delivery.maxAttempts} attempts so far${delivery.lastError ? `: ${delivery.lastError}` : ""})` : ""}. It
          must be carried out by {formatDateTime(delivery.notAfter)}, or not at all.
        </Card>
      );
    case "done":
      return (
        <Card className="bg-sunken p-4 text-[13.5px]">
          <p>
            Carried out by {host} {formatDateTime(delivery.finishedAt)}.
          </p>
          {answer ? <div className="text-muted">{answer}</div> : null}
        </Card>
      );
    case "refused":
      return (
        <Card className="border-danger/30 bg-danger-soft p-4 text-[13.5px] text-danger-ink">
          <p className="font-medium">Decided, but refused by the receiving system.</p>
          <p className="mt-1">{host} refused it and did nothing. The refusal is final: it is not sent again. If it is still needed, it must be proposed again.</p>
          {answer ? (
            <>
              <p className="mt-2">Its reason, as it gave it:</p>
              {answer}
            </>
          ) : null}
        </Card>
      );
    case "not_sent": {
      const deadline = toDate(delivery.sendAgainBefore);
      const canSendAgain = !!deadline && deadline > now;
      return (
        <Card className="border-danger/30 bg-danger-soft p-4 text-[13.5px] text-danger-ink">
          <p className="font-medium">Decided, but it never reached the receiving system.</p>
          <p className="mt-1">
            None of the {attempts} reached {host}
            {delivery.lastError ? `: ${delivery.lastError}` : ""}. Nothing was done there.{" "}
            {canSendAgain
              ? `It may be sent again until ${formatDateTime(delivery.sendAgainBefore)}.`
              : "It is too late to send it again; if it is still needed, it must be proposed again."}
          </p>
          {canSendAgain ? (
            <Button variant="secondary" size="sm" className="mt-3" onClick={() => sendAgain.mutate()} loading={sendAgain.isPending}>
              <RotateCw /> Send again
            </Button>
          ) : null}
        </Card>
      );
    }
    case "unknown": {
      const from = toDate(delivery.settleAfter);
      const canSettle = !!from && from <= now;
      return (
        <Card className="border-warning/30 bg-warning-soft p-4 text-[13.5px] text-warning-ink">
          <p className="font-medium">Unknown whether it was carried out.</p>
          <p className="mt-1">
            {host} may or may not have acted on it after {attempts}
            {delivery.lastError ? ` (${delivery.lastError})` : ""}. It is never sent again, since doing it twice could be worse than not at all. Check in{" "}
            {host} whether it happened, and settle it here.
          </p>
          {answer ? (
            <>
              <p className="mt-2">What it said:</p>
              {answer}
            </>
          ) : null}
          {canSettle ? (
            <div className="mt-3 flex flex-wrap gap-2">
              <Button variant="secondary" size="sm" onClick={() => settle.mutate(true)} loading={settle.isPending && settle.variables === true} disabled={settle.isPending}>
                <Check /> It was carried out
              </Button>
              <Button variant="secondary" size="sm" onClick={() => settle.mutate(false)} loading={settle.isPending && settle.variables === false} disabled={settle.isPending}>
                <X /> It was not
              </Button>
            </div>
          ) : (
            <p className="mt-2">It can be settled from {formatDateTime(delivery.settleAfter)}, once the receiving system can no longer carry it out.</p>
          )}
        </Card>
      );
    }
    case "settled_done":
    case "settled_not_done":
      return (
        <Card className="bg-sunken p-4 text-[13.5px]">
          <p>
            {delivery.status === "settled_done" ? "Carried out" : "Not carried out"}, as a person settled it after the outcome at {host} was unknown.
          </p>
          {answer ? <div className="text-muted">{answer}</div> : null}
        </Card>
      );
    default:
      return <Card className="bg-sunken p-4 text-[13.5px]">Delivery to {host}: {delivery.status}.</Card>;
  }
}

/**
 * The schema line of the checks. A pending proposal that waits because its
 * payload no longer fits says so, whatever is known about the action. A
 * decided proposal was checked against the schema as it was then, which need
 * not be the schema today.
 */
function schemaCheck(proposal: Proposal, hasSchema: boolean | undefined): { ok: boolean; text: string }[] {
  const pending = proposal.status === "pending";
  if (pending && proposal.skipReason === "payload_invalid") {
    return [{ ok: false, text: "The action's schema has changed since this was proposed, and the payload no longer fits it." }];
  }
  if (hasSchema === undefined) return [];
  if (!hasSchema) return [{ ok: false, text: "The action has no payload schema, so the payload's shape was not checked." }];
  return [{ ok: true, text: pending ? "The payload fits the action's schema." : "The payload fit the action's schema when it was proposed." }];
}

/**
 * What pgmandate itself checked, apart from what the agent claims.
 * hasSchema is undefined while the action is not known yet, and the
 * schema line is then left out rather than guessed.
 */
function Checks({ proposal, hasSchema }: { proposal: Proposal; hasSchema: boolean | undefined }) {
  const flagged = proposal.flags.length > 0;
  const items: { ok: boolean; text: string }[] = [
    { ok: true, text: `Proposed by ${proposal.agentName || shortId(proposal.agentId)}, with that agent's own token.` },
    ...schemaCheck(proposal, hasSchema),
    flagged
      ? { ok: false, text: "What the agent wrote holds text that tries to steer a model: the risk is high and no mandate allows it." }
      : { ok: true, text: "No known phrasing that tries to steer a model was found. This is a tripwire, not a guarantee." },
    { ok: true, text: `The risk, ${riskLabel[proposal.risk]?.toLowerCase() ?? proposal.risk}, is set by the action and these checks, not by the agent.` },
  ];
  return (
    <section aria-labelledby="checks-heading">
      <p id="checks-heading" className="label-mono mb-2 text-[12px] text-muted">
        Checked by pgmandate
      </p>
      <ul className="flex flex-col gap-1 text-[13.5px]">
        {items.map((item) => (
          <li key={item.text} className="flex items-start gap-2">
            {item.ok ? (
              <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" aria-label="Checked" />
            ) : (
              <ShieldAlert className="mt-0.5 size-4 shrink-0 text-danger" aria-label="Look closer" />
            )}
            <span>{item.text}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

/** The agent's account: its rationale, what it says it looked at, and who
 * wrote it. It argues for the proposal, so it comes after the facts,
 * folded, in the agent's colour, and hidden when it was flagged. */
function Account({ proposal }: { proposal: Proposal }) {
  const flagged = proposal.flags.length > 0;
  const [open, setOpen] = React.useState(false);
  const [revealed, setRevealed] = React.useState(false);
  // Revealing unmounts the button that had focus: the text it revealed takes it.
  const text = React.useRef<HTMLParagraphElement>(null);
  React.useEffect(() => {
    if (revealed) text.current?.focus();
  }, [revealed]);
  const provenance = proposal.provenance;
  const written = [
    provenance?.client ? `${provenance.client}${provenance.clientVersion ? ` ${provenance.clientVersion}` : ""}` : "",
    provenance?.model ? `model ${provenance.model}` : "",
    provenance?.agentVersion ? `agent ${provenance.agentVersion}` : "",
  ].filter(Boolean);
  const long = proposal.rationale.length > 400 || proposal.evidence.length > 0;
  return (
    <section aria-labelledby="account-heading">
      <p id="account-heading" className="label-mono mb-2 flex items-center gap-1.5 text-[12px] text-ai">
        <Bot className="size-3.5" aria-hidden /> Claimed by the agent, not checked
      </p>
      {!proposal.rationale ? (
        <p className="text-[13.5px] text-muted">No rationale: this was proposed before agents had to give one.</p>
      ) : flagged && !revealed ? (
        <Card className="bg-danger-soft p-4 text-[13.5px] text-danger-ink">
          The agent's account holds text that tries to steer a model, so it is hidden. Read it knowing that.
          <Button variant="secondary" size="sm" className="mt-3 block" onClick={() => setRevealed(true)}>
            Show the flagged text
          </Button>
        </Card>
      ) : (
        <Card className="border-l-2 border-l-ai bg-ai-soft/40 p-4">
          <p
            id="account-text"
            ref={text}
            tabIndex={-1}
            className={cn("whitespace-pre-wrap text-[14px] leading-relaxed outline-none", !open && long && "line-clamp-4")}
          >
            {proposal.rationale}
          </p>
          {open && proposal.evidence.length > 0 ? (
            <div className="mt-4">
              <p className="label-mono mb-2 text-[11px] text-muted">What it says it looked at</p>
              <ul className="flex flex-col gap-2">
                {proposal.evidence.map((e, i) => (
                  <li key={i} className="text-[13.5px]">
                    <span className="font-medium">{e.source}</span>
                    {e.ref ? <span className="ml-2 break-all font-mono text-[12px] text-muted select-all">{e.ref}</span> : null}
                    <p className="text-muted">{e.summary}</p>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          {long ? (
            <Button variant="ghost" size="sm" className="mt-2 -ml-2" aria-expanded={open} aria-controls="account-text" onClick={() => setOpen(!open)}>
              {open ? "Show less" : proposal.evidence.length > 0 ? `Read it all, with ${proposal.evidence.length} references` : "Read it all"}
            </Button>
          ) : null}
          <p className="mt-3 text-[12px] text-muted">
            Written by the agent{written.length ? ` (${written.join(", ")})` : ""}. The references are text to look up yourself, never links.
          </p>
        </Card>
      )}
    </section>
  );
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
  const dismiss = useMutation({
    mutationFn: () => api.proposals.dismissProposal({ id, note }),
    onSuccess: () => {
      toast.success("Dismissed");
      invalidate();
      onDecided();
    },
    onError: (error) => toast.error("Could not dismiss", { description: errorText(error) }),
  });

  if (one.isPending) return <Skeleton className="h-64" />;
  // A failed poll keeps the proposal it had, so a review in progress is
  // not taken away; only a first load that fails shows the error.
  if (!one.data) return <ErrorState error={one.error} onRetry={() => one.refetch()} />;
  const p = one.data.proposal!;
  const pending = p.status === "pending";
  const action = actions.data?.actions.find((a) => a.key === p.action);
  // The server says what risk an approval would run at, under the action
  // as it is now; empty when the action is gone and it can only be rejected.
  const approvalRisk = p.approvalRisk || p.risk;
  const raisedToHigh = p.risk !== "high" && approvalRisk === "high";
  const needsConfirm = approvalRisk === "high" || confirmAsked;
  const actionGone = pending && !p.approvalRisk;
  const decision = one.data.history.find((e) =>
    ["proposal.approved", "proposal.rejected", "proposal.withdrawn", "proposal.expired", "proposal.dismissed"].includes(e.type),
  );
  // A dry run is never carried out: a person only dismisses it.
  const dry = p.dryRun;
  return (
    <div className="flex flex-col gap-5">
      <Link to="/inbox" className="-my-2 flex min-h-11 items-center gap-1 text-[13px] text-muted hover:text-foreground lg:hidden">
        <ArrowLeft className="size-3.5" /> Back to the inbox
      </Link>
      {one.isError ? <StaleNote error={one.error} /> : null}
      <div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[12px] text-accent">{p.action}</span>
          <RiskBadge risk={p.risk} />
          <Badge tone={p.status === "pending" ? "warning" : ["rejected", "withdrawn", "expired", "dismissed"].includes(p.status) ? "neutral" : "success"}>
            {statusLabel[p.status] ?? p.status}
          </Badge>
          {dry ? <Badge tone="accent">dry run</Badge> : null}
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
          Proposed by {p.agentName || shortId(p.agentId)} · {formatDateTime(p.createdAt)}
          {pending ? ` · waits until ${formatDateTime(p.expiresAt)}` : ""}
          {p.runId ? (
            <span title="The agent's own id for the run this proposal belongs to">{` · run ${p.runId}`}</span>
          ) : null}
        </p>
      </div>

      {dry ? (
        <Card className="border-accent/40 p-4 text-[13.5px]">
          <p className="font-medium">A dry run: nothing here is carried out.</p>
          <p className="mt-1 text-muted">
            The agent is in dry run. It keeps its own credential and does this itself; pgmandate records what it would have decided. A dry run is never
            approved or rejected; dismiss it once you have looked at it. An owner makes the agent live under Agents &amp; actions.
          </p>
        </Card>
      ) : null}
      {p.delivery ? <DeliveryCard proposalId={p.id} delivery={p.delivery} onChanged={invalidate} /> : null}
      {pending && p.skipReason ? (
        <Card className="bg-warning-soft p-4 text-[13.5px] text-warning-ink">
          {dry ? "A person would have decided it" : "Waits for a person"}: {skipText(p.skipReason)}.
          {p.unmetConditions.length > 0 ? (
            <ul className="mt-2 flex flex-col gap-1 font-mono text-[12px]">
              {p.unmetConditions.map((line) => (
                <li key={line}>{line}</li>
              ))}
            </ul>
          ) : null}
        </Card>
      ) : null}
      {p.executionBasis ? (
        <Card className="bg-success-soft p-4 text-[13.5px] text-success">
          {dry ? "Would have been allowed without a person under" : "Allowed without a person under"}{" "}
          <Link to="/mandates" className="underline underline-offset-2">
            mandate {shortId(p.executionBasis.mandateId)}
          </Link>{" "}
          ({p.executionBasis.policyRef}), up to {riskLabel[p.executionBasis.maxRisk]?.toLowerCase()} risk.
          {p.executionBasis.budgets.map((b) => (
            <React.Fragment key={b.kind}>
              {" "}
              {Number(b.spent ?? 0)} of {executions(b.limit)} this month were used before this one{dry ? "; a dry run uses none" : ""}.
            </React.Fragment>
          ))}
          {p.executionBasis.condition ? (
            <>
              {" "}
              Its condition held: <code className="font-mono text-[12px]">{p.executionBasis.condition}</code>
            </>
          ) : null}
        </Card>
      ) : null}
      {decision ? (
        <Card className="bg-sunken p-4 text-[13.5px]">
          {p.status === "withdrawn"
            ? `Withdrawn by the agent, ${formatDateTime(p.decidedAt)}`
            : p.status === "expired"
            ? `Expired, ${formatDateTime(p.decidedAt)}: nobody decided it in time, and nothing was done`
            : p.status === "dismissed"
            ? `Dismissed by ${decision.actorName || "a person"}, ${formatDateTime(p.decidedAt)}`
            : `${statusLabel[p.status]} by ${decision.actorName || "a person"}, ${formatDateTime(p.decidedAt)}`}
          {p.decisionNote ? `: “${p.decisionNote}”` : "."}
        </Card>
      ) : null}

      <div>
        <p className="label-mono mb-2 text-[12px] text-muted">Where it goes</p>
        {/* Where an approval goes depends on the action, so nothing is
            claimed until the actions have loaded. A failed refetch keeps
            the data it had, and that is still shown. */}
        {!dry && !actionGone && !actions.data ? (
          actions.isError ? (
            <ErrorState title="Could not load the action" error={actions.error} onRetry={() => actions.refetch()} />
          ) : (
            <Skeleton className="h-5 w-2/3" />
          )
        ) : (
          <p className="text-[14px]">
            {dry
              ? "Nowhere: a dry run is never carried out by pgmandate."
              : actionGone
              ? "The action has been removed, so this can only be rejected."
              : action?.connector === "webhook" && action.connectorUrl
                ? `An approval is sent to ${hostOf(action.connectorUrl)}, which carries it out.`
                : "An approval is carried out by this server's own code."}
          </p>
        )}
      </div>

      <div>
        <div className="mb-2 flex items-center justify-between">
          <p className="label-mono text-[12px] text-muted">Payload</p>
          {pending && !dry ? (
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
              aria-describedby="payload-hint"
            />
            <p id="payload-hint" className={cn("mt-1 text-[12px]", payloadError ? "text-danger-ink" : "text-muted")}>
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

      <Checks proposal={p} hasSchema={actions.data ? !!action?.schemaJson : undefined} />
      <Account proposal={p} />

      {pending && dry ? (
        <div className="flex flex-col gap-3 border-t border-border pt-5">
          <Field label="Note" htmlFor="note" hint="Kept in the audit trail with the dismissal. Optional.">
            <Textarea id="note" value={note} onChange={(e) => setNote(e.target.value)} rows={2} />
          </Field>
          <div className="flex gap-2">
            <Button variant="secondary" onClick={() => dismiss.mutate()} loading={dismiss.isPending}>
              <X /> Dismiss
            </Button>
          </div>
        </div>
      ) : null}
      {pending && !dry ? (
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
              <Button variant="success" onClick={() => approve.mutate()} loading={approve.isPending} disabled={(needsConfirm && !confirm) || !!payloadError}
                aria-describedby={payloadError ? "payload-hint" : undefined}
              >
                <Check /> {changed ? "Approve with edits" : "Approve"}
              </Button>
            )}
            <Button
              variant="danger"
              className="ml-auto"
              onClick={() => reject.mutate()}
              loading={reject.isPending}
              disabled={!note.trim()}
              aria-describedby={note.trim() ? undefined : "reject-why"}
            >
              <X /> Reject
            </Button>
          </div>
          {note.trim() ? null : (
            <p id="reject-why" className="text-[12px] text-muted">
              A rejection needs a note.
            </p>
          )}
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
  const [tab, setTab] = React.useState("pending");
  const current = tabs.find((t) => t.key === tab) ?? tabs[0];
  // What waits is a queue, worked oldest first; the rest reads newest first.
  // The dry-run tab waits too, so this goes by status, not by tab.
  const oldestFirst = current.status === "pending";
  const list = useQuery({
    queryKey: ["proposals", tab],
    queryFn: () => api.proposals.listProposals({ status: current.status, dryRun: current.dryRun, limit: 200, oldestFirst }),
    refetchInterval: 10_000,
  });
  const proposals = list.data?.proposals ?? [];
  const total = Number(list.data?.total ?? 0);
  const allActions = useQuery({ queryKey: ["actions", "all"], queryFn: () => api.actions.listActions({ includeRemoved: true }) });
  const descriptions = new Map((allActions.data?.actions ?? []).map((a) => [a.key, a.description]));
  // The panel takes focus when the last waiting proposal is decided, so a
  // keyboard user is not left on the page itself.
  const focusPanel = () => window.setTimeout(() => document.getElementById("inbox-panel")?.focus(), 0);
  const chooseTab = (key: string) => {
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
    if (current.status === "pending" && after.length) {
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
            ) : !list.data ? (
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
            {list.isError && list.data ? <StaleNote error={list.error} className="mt-2" /> : null}
            {total > proposals.length ? (
              <p className="mt-2 text-[12px] text-muted">
                Showing {proposals.length} of {total}, {oldestFirst ? "oldest" : "newest"} first.
              </p>
            ) : null}
          </div>
          <div>{id ? <Detail key={id} id={id} onDecided={next} /> : <p className="hidden pt-10 text-center text-sm text-muted lg:block">Select a proposal.</p>}</div>
        </div>
      </div>
    </div>
  );
}
