import { useQuery } from "@tanstack/react-query";
import { Download, FileCheck, ShieldAlert, ShieldCheck } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";
import { PageHeader } from "@/components/layout/AppShell";
import { Button } from "@/components/ui/button";
import { Card, EmptyState, ErrorState, Field, Input, Skeleton } from "@/components/ui/primitives";
import { api } from "@/lib/api";
import { errorText } from "@/lib/errors";
import { Link } from "react-router";
import { actorLabel, eventLabel, formatDateTime, prettyJson } from "@/lib/format";
import { useSession } from "@/lib/session";

function download(name: string, text: string) {
  const blob = new Blob([text], { type: "application/x-ndjson" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

export function EvidencePage() {
  const session = useSession();
  const [subject, setSubject] = React.useState("");
  const [applied, setApplied] = React.useState("");
  const chain = useQuery({ queryKey: ["chain"], queryFn: () => api.evidence.verifyChain({}) });
  const events = useQuery({
    queryKey: ["events", applied],
    queryFn: () => api.evidence.listEvents({ subjectId: applied, limit: 200 }),
    refetchInterval: 15_000,
  });
  const [busy, setBusy] = React.useState<string | null>(null);
  async function exportIt(kind: "chain" | "decisions") {
    setBusy(kind);
    try {
      const prefix = (session.tenant?.tenantName ?? "pgmandate").replace(/[^a-z0-9]+/gi, "-").toLowerCase();
      if (kind === "chain") {
        const response = await api.evidence.exportChain({});
        download(`${prefix}-chain.jsonl`, response.jsonl);
      } else {
        const response = await api.evidence.exportDecisions({});
        download(`${prefix}-decisions.jsonl`, response.jsonl);
      }
    } catch (error) {
      toast.error("Could not export", { description: errorText(error) });
    } finally {
      setBusy(null);
    }
  }
  const list = events.data?.events ?? [];
  return (
    <div>
      <PageHeader
        eyebrow={
          <>
            <FileCheck className="size-3.5" /> The audit trail
          </>
        }
        title="Evidence"
      >
        <Button variant="secondary" onClick={() => exportIt("chain")} loading={busy === "chain"}>
          <Download /> Export the chain
        </Button>
        <Button variant="secondary" onClick={() => exportIt("decisions")} loading={busy === "decisions"}>
          <Download /> Export decisions
        </Button>
      </PageHeader>
      <div className="max-w-6xl px-6 py-6 lg:px-8">
        <Card className="mb-6 flex flex-wrap items-center gap-3 p-5">
          {chain.data?.ok ? <ShieldCheck className="size-5 text-success" /> : chain.data ? <ShieldAlert className="size-5 text-danger" /> : null}
          <div className="flex-1 text-[14px]">
            {chain.isPending ? (
              "Checking the audit trail…"
            ) : chain.isError ? (
              "Could not check the audit trail right now."
            ) : chain.data.ok ? (
              <>
                <span className="font-medium">Audit trail verified.</span> {Number(chain.data.events)} events, each linked to the one before it by a
                hash, so an edit or a removal would show. "Export the chain" gives every event with its hashes; it verifies without this server:{" "}
                <code className="font-mono text-[12px]">pgmandate verify-export -tenant {session.tenant?.tenantId} -file chain.jsonl</code>. "Export
                decisions" gives each decided proposal with its outcome.
              </>
            ) : (
              <>
                <span className="font-medium text-danger">The audit trail is broken</span> at {chain.data.error}.
              </>
            )}
          </div>
        </Card>
        <form
          className="mb-4 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            setApplied(subject.trim());
          }}
        >
          <Field label="Subject" htmlFor="subject" hint="A proposal, agent, mandate or action id. Empty shows the newest events." className="w-full max-w-md">
            <Input id="subject" value={subject} onChange={(e) => setSubject(e.target.value)} placeholder="uuid" />
          </Field>
          <Button type="submit" variant="secondary">
            Filter
          </Button>
        </form>
        {events.isPending ? (
          <Skeleton className="h-40" />
        ) : events.isError ? (
          <ErrorState error={events.error} onRetry={() => events.refetch()} />
        ) : list.length === 0 ? (
          <EmptyState icon={<FileCheck />} title="No events" />
        ) : (
          <Card className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-[13px]">
              <thead>
                <tr className="label-mono border-b border-border text-left text-[11px] text-muted">
                  <th className="px-4 py-2">#</th>
                  <th className="px-2 py-2">When</th>
                  <th className="px-2 py-2">Actor</th>
                  <th className="px-2 py-2">Event</th>
                  <th className="px-2 py-2">Subject</th>
                  <th className="px-2 py-2">Run</th>
                  <th className="px-2 py-2">Details</th>
                </tr>
              </thead>
              <tbody>
                {list.map((e) => (
                  <tr key={e.seq} className="border-b border-border align-top last:border-b-0">
                    <td className="px-4 py-2 font-mono text-[12px] text-muted">{Number(e.seq)}</td>
                    <td className="whitespace-nowrap px-2 py-2 text-muted">{formatDateTime(e.createdAt)}</td>
                    <td className="px-2 py-2">
                      {actorLabel[e.actorKind] ?? e.actorKind}
                      {e.actorName ? ` ${e.actorName}` : ""}
                    </td>
                    <td className="px-2 py-2" title={e.type}>
                      {e.type.startsWith("proposal.") || e.type.startsWith("delivery.") ? (
                        <Link to={`/inbox/${e.subjectId}`} className="underline-offset-2 hover:underline">
                          {eventLabel[e.type] ?? e.type}
                        </Link>
                      ) : (
                        (eventLabel[e.type] ?? e.type)
                      )}
                    </td>
                    <td className="px-2 py-2 font-mono text-[12px] text-muted">
                      {e.subjectId ? (
                        <button type="button" className="underline-offset-2 hover:underline" onClick={() => { setSubject(e.subjectId); setApplied(e.subjectId); }}>
                          {e.subjectId.slice(0, 8)}
                        </button>
                      ) : (
                        ""
                      )}
                    </td>
                    <td className="px-2 py-2 font-mono text-[12px] text-muted">{e.runId}</td>
                    <td className="max-w-[320px] px-2 py-2">
                      <details>
                        <summary className="cursor-pointer text-[12px] text-muted">Show</summary>
                        <pre className="mt-1 whitespace-pre-wrap break-all font-mono text-[12px] text-muted">{prettyJson(e.payloadJson)}</pre>
                      </details>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        )}
      </div>
    </div>
  );
}
