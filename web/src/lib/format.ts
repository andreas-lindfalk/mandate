import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { timestampDate } from "@bufbuild/protobuf/wkt";

const dateTime = new Intl.DateTimeFormat("en-GB", { dateStyle: "medium", timeStyle: "short" });
const dateOnly = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", year: "numeric" });
const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

export function toDate(ts?: Timestamp): Date | null {
  return ts ? timestampDate(ts) : null;
}

export function formatDateTime(ts?: Timestamp) {
  const date = toDate(ts);
  return date ? dateTime.format(date) : "–";
}

export function formatDate(ts?: Timestamp) {
  const date = toDate(ts);
  return date ? dateOnly.format(date) : "–";
}

export function formatRelative(ts?: Timestamp) {
  const date = toDate(ts);
  if (!date) return "";
  const minutes = Math.round((date.getTime() - Date.now()) / 60000);
  if (Math.abs(minutes) < 60) return relative.format(minutes, "minute");
  const hours = Math.round(minutes / 60);
  if (Math.abs(hours) < 24) return relative.format(hours, "hour");
  return relative.format(Math.round(hours / 24), "day");
}

export const riskLabel: Record<string, string> = { low: "Low", medium: "Medium", high: "High" };

export const riskTone: Record<string, "success" | "warning" | "danger"> = { low: "success", medium: "warning", high: "danger" };

export const statusLabel: Record<string, string> = {
  pending: "Waiting",
  approved: "Approved",
  rejected: "Rejected",
  executed: "Allowed by mandate",
  withdrawn: "Withdrawn",
  expired: "Expired",
};

export const flagLabel: Record<string, string> = {
  steering_text: "Text that tries to steer a model",
};

export const actorLabel: Record<string, string> = { person: "Person", agent: "Agent", system: "pgmandate" };

/** Plain words for the events in the audit trail. */
export const eventLabel: Record<string, string> = {
  "tenant.created": "Tenant created",
  "person.added": "Person added",
  "person.removed": "Person removed",
  "agent.created": "Agent registered",
  "agent.revoked": "Agent revoked",
  "agent.token_issued": "Token issued",
  "agent.token_revoked": "Token revoked",
  "agent.tool_called": "Agent called a tool",
  "action.defined": "Action defined",
  "action.changed": "Action changed",
  "action.removed": "Action removed",
  "mandate.granted": "Mandate granted",
  "mandate.revoked": "Mandate revoked",
  "proposal.created": "Proposal made",
  "proposal.approved": "Approved by a person",
  "proposal.rejected": "Rejected by a person",
  "proposal.executed": "Allowed by a mandate",
  "proposal.execution_skipped": "Left for a person",
  "proposal.rapid_approvals": "Many approvals in a short time",
  "proposal.withdrawn": "Withdrawn by the agent",
  "proposal.expired": "Expired: nobody decided in time",
  "proposal.notified": "Chat channel told",
  "agent.limits_set": "Agent's limits set",
  "notifier.set": "Notifications set",
  "notifier.removed": "Notifications turned off",
  "delivery.retargeted": "Delivery moved with its action",
  "delivery.delivered": "Delivered",
  "delivery.failed": "Delivery failed",
  "delivery.retried": "Delivery sent again",
};

/** The host of a URL, for showing where something goes without the whole address. */
export function hostOf(url: string) {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** A length of time in whole seconds, in the largest unit that divides it: "7 days", "90 minutes". */
export function durationLabel(seconds: number | bigint) {
  const s = Number(seconds);
  for (const [size, name] of [
    [86400, "day"],
    [3600, "hour"],
    [60, "minute"],
  ] as const) {
    if (s >= size && s % size === 0) {
      const n = s / size;
      return `${n} ${name}${n === 1 ? "" : "s"}`;
    }
  }
  return `${s} second${s === 1 ? "" : "s"}`;
}

/** What a budget allows and has used, from an execution's basis or a mandate. */
export function executions(n: number | bigint) {
  const value = Number(n);
  return `${value} ${value === 1 ? "execution" : "executions"}`;
}

export const skipLabel: Record<string, string> = {
  no_mandate: "No mandate covers this action for the agent",
  risk_above_mandate: "The risk is above what the mandate allows",
  budget_spent: "The mandate's budget for this month is spent",
  condition_not_met: "The payload does not meet the mandate's condition",
  flagged: "Flagged: the proposal carries text that tries to steer a model",
  action_not_executable: "This action is never carried out without a person",
  action_failed: "The system that carries it out refused it",
  execution_failed: "The execution failed",
  agent_revoked: "The agent is revoked",
  action_removed: "The action has been removed; this can only be rejected",
  action_changed: "The action's risk was raised since it was proposed",
  payload_invalid: "The action's schema has changed and the payload no longer fits it",
};

export const roleLabel: Record<string, string> = { owner: "Owner", reviewer: "Reviewer" };

/** Pretty JSON, or the text as is when it is not JSON. */
export function prettyJson(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

export function shortId(id: string) {
  return id.slice(0, 8);
}
