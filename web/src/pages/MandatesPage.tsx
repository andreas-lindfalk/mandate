import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, ScrollText } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogTrigger } from "@/components/ui/dialog";
import { Badge, Card, EmptyState, ErrorState, Field, Input, Skeleton, StaleNote, Textarea } from "@/components/ui/primitives";
import { Select } from "@/components/ui/select";
import type { Mandate } from "@/gen/mandate/v1/mandates_pb";
import { api } from "@/lib/api";
import { errorText } from "@/lib/errors";
import { singleClick, useFocusBack } from "@/lib/utils";
import { durationLabel, executions, formatDate, riskLabel, riskTone, shortId } from "@/lib/format";
import { useSession } from "@/lib/session";

/** Whether a mandate pattern covers an action key: the server's rule. */
function covers(pattern: string, key: string) {
  if (pattern === "*" || pattern === key) return true;
  return pattern.endsWith(".*") && key.startsWith(pattern.slice(0, -1));
}

function GrantDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = React.useState(false);
  const agents = useQuery({ queryKey: ["agents"], queryFn: () => api.agents.listAgents({}) });
  const actions = useQuery({ queryKey: ["actions"], queryFn: () => api.actions.listActions({}) });
  const [agentId, setAgentId] = React.useState("");
  const [picked, setPicked] = React.useState<string[]>([]);
  const [pattern, setPattern] = React.useState("");
  const [maxRisk, setMaxRisk] = React.useState("low");
  const [budget, setBudget] = React.useState("100");
  const [policy, setPolicy] = React.useState("");
  const [days, setDays] = React.useState("90");
  const [department, setDepartment] = React.useState("");
  const [condition, setCondition] = React.useState("");
  // The terms the person said they are accountable for; ticked only while
  // the terms are still those.
  const [accountableFor, setAccountableFor] = React.useState<string | null>(null);

  // Every opening starts empty: a grant cancelled half way never comes
  // back with its actions still ticked.
  const onOpenChange = (next: boolean) => {
    if (next) {
      setAgentId("");
      setPicked([]);
      setPattern("");
      setMaxRisk("low");
      setBudget("100");
      setPolicy("");
      setDays("90");
      setDepartment("");
      setCondition("");
      setAccountableFor(null);
    }
    setOpen(next);
  };

  // "I am accountable" is for the terms as they were when it was ticked;
  // any change to them asks for it again.
  const terms = [agentId, picked.join(","), pattern, maxRisk, budget, policy, days, condition.trim()].join("|");
  const accountable = accountableFor === terms;

  const patterns = picked.concat(pattern.split(",").map((e) => e.trim()).filter(Boolean));
  const defined = actions.data?.actions ?? [];
  const rank: Record<string, number> = { low: 0, medium: 1, high: 2 };
  // What the mandate will really let through: covered, delegable, and within the risk.
  const covered = defined.filter((a) => patterns.some((p) => covers(p, a.key)));
  const effective = covered.filter((a) => a.mayExecute && rank[a.risk] <= rank[maxRisk]);
  const agentName = agentId === "*" ? "Every agent of the tenant" : ((agents.data?.agents ?? []).find((a) => a.id === agentId)?.name ?? "");
  // The preview speaks for what is defined today, so it and the grant wait
  // for both lists: an empty list while loading, or after a failed load,
  // would read as "nothing is defined".
  const loaded = !!actions.data && !!agents.data;
  const ready = loaded && !!agentId && patterns.length > 0 && !!policy.trim() && Number(budget) > 0 && accountable;

  const grant = useMutation({
    mutationFn: () =>
      api.mandates.grantMandate({
        agentId: agentId === "*" ? "" : agentId,
        actions: patterns,
        maxRisk,
        budgetCount: BigInt(Number(budget) || 0),
        policyRef: policy,
        days: Number(days),
        department,
        condition,
      }),
    onSuccess: () => {
      toast.success("Mandate granted");
      setOpen(false);
      setAccountableFor(null);
      onDone();
    },
    onError: (error) => {
      if (!isConditionError(errorText(error))) toast.error("Could not grant", { description: errorText(error) });
    },
  });
  // A refusal of the condition is shown at the field, where it can be fixed:
  // only when there is one, and the server's message is about it.
  const isConditionError = (text: string) => !!condition.trim() && /\bcondition\b/i.test(text);
  const refusal = grant.isError ? errorText(grant.error) : "";
  const conditionError = isConditionError(refusal) ? refusal : "";
  const agentOptions = (agents.data?.agents ?? [])
    .filter((a) => !a.revokedAt)
    .map((a) => ({ value: a.id, label: a.name }))
    .concat([{ value: "*", label: "Every agent of the tenant" }]);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>
        <Button>
          <Plus /> Grant a mandate
        </Button>
      </DialogTrigger>
      <DialogContent
        className="w-[min(820px,calc(100vw-32px))]"
        title="Grant a mandate"
        description="A mandate lets an agent have actions carried out without anyone looking, within the limits you set here."
      >
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (ready) grant.mutate();
          }}
        >
          <div className="grid gap-x-6 gap-y-4 md:grid-cols-2">
            <div className="flex flex-col gap-4">
          <Field label="Agent" htmlFor="agent">
            {agents.data ? (
              <Select id="agent" value={agentId} onValueChange={setAgentId} options={agentOptions} placeholder="Choose an agent" />
            ) : agents.isError ? (
              <ErrorState className="py-4" title="Could not load the agents" error={agents.error} onRetry={() => agents.refetch()} />
            ) : (
              <Skeleton className="h-9" />
            )}
          </Field>
          {agents.isError && agents.data ? <StaleNote error={agents.error} className="-mt-2" /> : null}
          <fieldset>
            <legend className="mb-1.5 text-[13px] font-medium">Actions</legend>
            {!actions.data ? (
              actions.isError ? (
                <ErrorState className="py-4" title="Could not load the actions" error={actions.error} onRetry={() => actions.refetch()} />
              ) : (
                <Skeleton className="h-20" />
              )
            ) : defined.length === 0 ? (
              <p className="text-[13px] text-muted">No actions are defined yet. Define one under Agents & actions first.</p>
            ) : (
              <ul className="flex flex-col gap-1.5">
                {defined.map((a) => {
                  const never = !a.mayExecute || a.risk === "high";
                  return (
                    <li key={a.key}>
                      <label className={never ? "flex items-center gap-2 text-[13.5px] text-muted" : "flex items-center gap-2 text-[13.5px]"}>
                        <input
                          type="checkbox"
                          disabled={never}
                          checked={picked.includes(a.key)}
                          onChange={(e) => setPicked(e.target.checked ? picked.concat(a.key) : picked.filter((k) => k !== a.key))}
                        />
                        <span className="font-mono text-[12.5px]">{a.key}</span>
                        <Badge tone={riskTone[a.risk] ?? "neutral"}>{riskLabel[a.risk]}</Badge>
                        <span className="text-muted">{never ? "always waits for a person" : `waits up to ${durationLabel(a.waitSeconds)} for a person otherwise`}</span>
                      </label>
                    </li>
                  );
                })}
              </ul>
            )}
            {actions.isError && actions.data ? <StaleNote error={actions.error} className="mt-1.5" /> : null}
            <details className="mt-2">
              <summary className="cursor-pointer text-[12.5px] text-muted">Advanced: a pattern</summary>
              <Input
                className="mt-1.5"
                value={pattern}
                onChange={(e) => setPattern(e.target.value)}
                placeholder="tickets.*"
                aria-label="Action patterns, comma separated"
              />
              <p className="mt-1 text-xs text-muted">A key, a prefix such as tickets.*, or * for every action, including ones defined later.</p>
            </details>
          </fieldset>
            </div>
            <div className="flex flex-col gap-4">
          <div className="grid grid-cols-2 gap-4">
            <Field label="Up to risk" htmlFor="risk" hint="High is never delegated.">
              <Select
                id="risk"
                value={maxRisk}
                onValueChange={setMaxRisk}
                options={[
                  { value: "low", label: "Low" },
                  { value: "medium", label: "Medium" },
                ]}
              />
            </Field>
            <Field label="Budget, executions per month" htmlFor="budget" hint="Once this many have run this month, further proposals wait for a person.">
              <Input id="budget" type="number" min={1} value={budget} onChange={(e) => setBudget(e.target.value)} />
            </Field>
          </div>
          <Field label="Policy reference" htmlFor="policy" hint="The document this mandate rests on, such as AI-POLICY-2026-01 §4. Recorded with every decision; the document itself is not stored or checked.">
            <Input id="policy" value={policy} onChange={(e) => setPolicy(e.target.value)} />
          </Field>
          <div className="grid grid-cols-2 gap-4">
            <Field label="Valid for, days" htmlFor="days">
              <Input id="days" type="number" min={1} max={366} value={days} onChange={(e) => setDays(e.target.value)} />
            </Field>
            <Field label="Department" htmlFor="department" hint="A label for finding things. It limits nothing yet.">
              <Input id="department" value={department} onChange={(e) => setDepartment(e.target.value)} placeholder="optional" />
            </Field>
          </div>
            </div>
          </div>
          <Field
            label="Condition on the payload, optional"
            htmlFor="condition"
            hint={
              'Narrows the mandate to proposals whose payload meets it, such as payload.amount <= 500 && payload.currency in ["EUR", "SEK"]. ' +
              "Fields are compared with values (== != < <= > >= in) and combined with && || !. The database checks it again on every use."
            }
          >
            <Textarea
              id="condition"
              value={condition}
              onChange={(e) => {
                setCondition(e.target.value);
                if (grant.isError) grant.reset();
              }}
              rows={2}
              className="font-mono text-[12.5px]"
              placeholder="payload.amount <= 500"
              spellCheck={false}
              aria-invalid={!!conditionError}
              aria-errormessage={conditionError ? "condition-error" : undefined}
              aria-describedby={conditionError ? "condition-error" : undefined}
            />
          </Field>
          {conditionError ? (
            <p id="condition-error" role="alert" className="-mt-2 overflow-x-auto whitespace-pre font-mono text-[12px] text-danger-ink">
              {conditionError}
            </p>
          ) : null}
          {agentId && patterns.length > 0 && !loaded && !actions.isError && !agents.isError ? <Skeleton className="h-28" /> : null}
          {agentId && patterns.length > 0 && loaded ? (
            <Card className="bg-warning-soft p-4 text-[13.5px] text-warning-ink">
              <p>
                <span className="font-medium">{agentName}</span> will have{" "}
                {effective.length ? effective.map((a) => a.key).join(", ") : "nothing that is defined today"} carried out{" "}
                <span className="font-medium">without anyone looking</span>, up to {maxRisk} risk and {executions(Number(budget) || 0)} a month, for {days} days
                {policy.trim() ? `, under ${policy.trim()}` : ""}
                {condition.trim() ? (
                  <>
                    , and only when the payload meets <code className="font-mono text-[12px]">{condition.trim()}</code>
                  </>
                ) : (
                  <>
                    , <span className="font-medium">whatever the payload says</span>: no condition limits an amount or any other field
                  </>
                )}
                . The budget counts executions, not money.
              </p>
              {patterns.includes("*") ? <p className="mt-1">The pattern * also covers every action defined later.</p> : null}
              {covered.length > effective.length ? (
                <p className="mt-1">
                  Covered but still decided by a person: {covered.filter((a) => !effective.includes(a)).map((a) => a.key).join(", ")}.
                </p>
              ) : null}
              <label className="mt-3 flex items-center gap-2 font-medium">
                <input type="checkbox" checked={accountable} onChange={(e) => setAccountableFor(e.target.checked ? terms : null)} />I am accountable for this delegation
              </label>
            </Card>
          ) : null}
          <div className="flex justify-end gap-2">
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={grant.isPending} disabled={!ready}>
              Grant mandate
            </Button>
          </div>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function MandateRow({
  mandate,
  agentName,
  agentRevoked,
  canRevoke,
  onChanged,
}: {
  mandate: Mandate;
  agentName: string;
  agentRevoked: boolean;
  canRevoke: boolean;
  onChanged: () => void;
}) {
  const [confirming, setConfirming] = React.useState(false);
  const revokeTrigger = useFocusBack(confirming);
  const revoke = useMutation({
    mutationFn: () => api.mandates.revokeMandate({ id: mandate.id }),
    onSuccess: () => {
      toast.success("Mandate revoked");
      onChanged();
    },
    onError: (error) => toast.error("Could not revoke", { description: errorText(error) }),
  });
  const spent = Number(mandate.spentCount);
  const budget = Number(mandate.budgetCount);
  const share = budget ? Math.min(100, Math.round((spent / budget) * 100)) : 0;
  // The clock is read once per mount: a mandate that expires while the page is open shows on the next refresh.
  const [now] = React.useState(() => new Date());
  const expired = !!mandate.validUntil && timestampDate(mandate.validUntil) < now;
  // A revoked agent can no longer propose, so its mandates can no longer
  // apply, though the server still lists them as not revoked and not expired.
  const ended = !!mandate.revokedAt || expired || agentRevoked;
  return (
    <li className="border-t border-border px-5 py-4 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <p className="font-medium">{agentName}</p>
        <span className="font-mono text-[12px] text-accent">{mandate.actions.join(", ")}</span>
        <Badge tone={mandate.maxRisk === "medium" ? "warning" : "success"}>up to {riskLabel[mandate.maxRisk]?.toLowerCase()} risk</Badge>
        {mandate.department ? <Badge tone="neutral">{mandate.department}</Badge> : null}
        {mandate.revokedAt ? (
          <Badge tone="neutral">revoked</Badge>
        ) : expired ? (
          <Badge tone="neutral">expired</Badge>
        ) : agentRevoked ? (
          <Badge tone="neutral">agent revoked</Badge>
        ) : null}
        {canRevoke && !ended ? (
          <div className="ml-auto flex gap-2">
            {confirming ? (
              <>
                {/* Cancel takes the trigger's place, so the click that armed this never lands on the danger button. */}
                <Button variant="danger" size="sm" onClick={singleClick(() => revoke.mutate())} loading={revoke.isPending}>
                  Revoke now
                </Button>
                <Button variant="ghost" size="sm" autoFocus onClick={() => setConfirming(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button variant="secondary" size="sm" ref={revokeTrigger()} onClick={() => setConfirming(true)}>
                Revoke
              </Button>
            )}
          </div>
        ) : null}
      </div>
      {mandate.condition ? (
        <p className="mt-1 text-[13px]">
          <span className="text-muted">Only when </span>
          <code className="font-mono text-[12px]">{mandate.condition}</code>
        </p>
      ) : null}
      <p className="mt-1 text-[12.5px] text-muted">
        Policy {mandate.policyRef} · valid {formatDate(mandate.validFrom)} to {formatDate(mandate.validUntil)} · {shortId(mandate.id)}
      </p>
      <div className="mt-2 flex items-center gap-3">
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-sunken" role="progressbar" aria-valuenow={share} aria-valuemin={0} aria-valuemax={100} aria-label="Budget used this month">
          <div className={share >= 100 ? "h-full bg-danger" : share >= 80 ? "h-full bg-warning" : "h-full bg-success"} style={{ width: `${share}%` }} />
        </div>
        <span className="font-mono text-[12px] text-muted">
          {spent} / {executions(budget)} this month
        </span>
      </div>
    </li>
  );
}

export function MandatesPage() {
  const session = useSession();
  const queryClient = useQueryClient();
  const [includeEnded, setIncludeEnded] = React.useState(false);
  const mandates = useQuery({ queryKey: ["mandates", includeEnded], queryFn: () => api.mandates.listMandates({ includeEnded }) });
  const agents = useQuery({ queryKey: ["agents"], queryFn: () => api.agents.listAgents({}) });
  const names = new Map((agents.data?.agents ?? []).map((a) => [a.id, a.name]));
  const revokedAgents = new Set((agents.data?.agents ?? []).filter((a) => !!a.revokedAt).map((a) => a.id));
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["mandates"] });
    queryClient.invalidateQueries({ queryKey: ["chain"] });
  };
  const list = mandates.data?.mandates ?? [];
  return (
    <div>
      <PageHeader
        eyebrow={
          <>
            <ScrollText className="size-3.5" /> Standing permissions
          </>
        }
        title="Mandates"
      >
        <label className="mr-2 flex items-center gap-2 text-[13px] text-muted">
          <input type="checkbox" checked={includeEnded} onChange={(e) => setIncludeEnded(e.target.checked)} /> Show ended
        </label>
        {session.isOwner ? <GrantDialog onDone={refresh} /> : null}
      </PageHeader>
      <div className="max-w-5xl px-6 py-6 lg:px-8">
        <p className="mb-6 max-w-2xl text-[15px] leading-relaxed text-foreground/85">
          A mandate lets an agent have actions carried out without a person: up to a risk, within a budget per calendar month, resting on a
          policy reference, until a date. Everything outside a mandate waits in the inbox, and the database checks each use of a mandate
          against what is written here.{session.isOwner ? "" : " You are a reviewer here; owners grant and revoke mandates."}
        </p>
        {mandates.isError && mandates.data ? <StaleNote error={mandates.error} className="mb-2" /> : null}
        {mandates.isPending ? (
          <Skeleton className="h-32" />
        ) : !mandates.data ? (
          <ErrorState error={mandates.error} onRetry={() => mandates.refetch()} />
        ) : list.length === 0 ? (
          <EmptyState icon={<ScrollText />} title="No mandates">
            Without a mandate every proposal waits for a person. {session.isOwner ? "Grant one above." : "An owner grants them."}
          </EmptyState>
        ) : (
          <Card>
            <ul>
              {list.map((m) => (
                <MandateRow
                  key={m.id}
                  mandate={m}
                  agentName={m.agentId ? (names.get(m.agentId) ?? shortId(m.agentId)) : "Every agent"}
                  agentRevoked={!!m.agentId && revokedAgents.has(m.agentId)}
                  canRevoke={session.isOwner}
                  onChanged={refresh}
                />
              ))}
            </ul>
          </Card>
        )}
      </div>
    </div>
  );
}
