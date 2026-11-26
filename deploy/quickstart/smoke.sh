#!/usr/bin/env bash
# The quickstart, end to end, through the public APIs only: what a newcomer
# does in the UI, as calls. CI runs it on a fresh quickstart; it can also be
# run by hand after `docker compose -f deploy/quickstart/compose.yaml up -d`.
#
# Needs curl and jq.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
compose=(docker compose -f "$here/compose.yaml")
url="${PGMANDATE_URL:-http://localhost:8585}"

fail() { echo "smoke: $*" >&2; exit 1; }

# call PATH TOKEN BODY [TENANT]: a Connect call in JSON.
call() {
  local path="$1" token="$2" body="$3" tenant="${4:-}"
  local headers=(-H "Content-Type: application/json")
  [ -n "$token" ] && headers+=(-H "Authorization: Bearer $token")
  [ -n "$tenant" ] && headers+=(-H "Mandate-Tenant: $tenant")
  curl -sS --fail-with-body "${headers[@]}" -d "$body" "$url$path"
}

echo "smoke: waiting for the server"
for _ in $(seq 1 90); do
  curl -sf "$url/healthz" >/dev/null 2>&1 && break
  sleep 2
done
curl -sf "$url/healthz" >/dev/null || fail "the server did not come up"

echo "smoke: the setup link, from the log"
link="$("${compose[@]}" logs pgmandate 2>&1 | grep -o "$url/setup#[A-Za-z0-9_-]*" | tail -1)"
[ -n "$link" ] || fail "no setup link in the log"
setup_token="${link#*#}"
pending="$(call /mandate.v1.AuthService/SignInMethods "" '{}' | jq -r '.setupPending')"
[ "$pending" = "true" ] || fail "the sign-in page does not say setup is pending"

session="$(call /mandate.v1.AuthService/SetUp "" "$(jq -n --arg t "$setup_token" '{token: $t, organisation: "Smoke Co", name: "Sam", email: "sam@smoke.example"}')" | jq -r '.token')"
[ -n "$session" ] && [ "$session" != "null" ] || fail "setup gave no session"
if call /mandate.v1.AuthService/SetUp "" "$(jq -n --arg t "$setup_token" '{token: $t, organisation: "Again", name: "Sam", email: "sam@smoke.example"}')" >/dev/null 2>&1; then
  fail "the setup link worked twice"
fi
tenant="$(call /mandate.v1.AuthService/Me "$session" '{}' | jq -r '.tenants[0].tenantId')"
echo "smoke: set up tenant $tenant"

echo "smoke: an agent, an action, a proposal and a decision"
agent="$(call /mandate.v1.AgentService/CreateAgent "$session" '{"name": "smoke-bot", "mode": "live"}' "$tenant" | jq -r '.agent.id')"
agent_token="$(call /mandate.v1.AgentService/IssueToken "$session" "$(jq -n --arg a "$agent" '{agentId: $a, days: 1}')" "$tenant" | jq -r '.secret')"
# The action is delivered to a receiver in the same container, as the
# README's quickstart does it.
audience="$(call /mandate.v1.ActionService/DefineAction "$session" '{"key": "tickets.create", "description": "Open a ticket", "risk": "low",
  "schemaJson": "{\"type\":\"object\",\"required\":[\"subject\"],\"properties\":{\"subject\":{\"type\":\"string\"}}}",
  "connector": "webhook", "connectorUrl": "http://127.0.0.1:8586/"}' "$tenant" | jq -r '.action.audience')"
[ -n "$audience" ] && [ "$audience" != "null" ] || fail "the action has no audience"
"${compose[@]}" exec -d pgmandate /pgmandate receiver -tenant "$tenant" -audience "$audience"
proposal="$(call /mandate.agent.v1.AgentService/Propose "$agent_token" '{"action": "tickets.create", "title": "Printer jams",
  "payloadJson": "{\"subject\":\"Printer jams\"}", "idempotencyKey": "smoke:1", "rationale": "The smoke test proposes."}' | jq -r '.proposal.proposalId')"
[ -n "$proposal" ] && [ "$proposal" != "null" ] || fail "the agent's proposal was not made"
status="$(call /mandate.v1.ProposalService/ApproveProposal "$session" "$(jq -n --arg p "$proposal" '{id: $p, note: "Approved by the smoke test."}')" "$tenant" | jq -r '.proposal.status')"
[ "$status" = "approved" ] || fail "the approval answered $status"
echo "smoke: the receiver carries it out, and the agent reads its result"
delivery=""
# The receiver may still be starting: an attempt it misses is retried.
for _ in $(seq 1 90); do
  read="$(call /mandate.agent.v1.AgentService/GetProposal "$agent_token" "$(jq -n --arg p "$proposal" '{proposalId: $p}')")"
  delivery="$(jq -r '.proposal.delivery.status' <<<"$read")"
  # Anything but pending is final: done, or a failure to report at once.
  [ "$delivery" != "pending" ] && [ "$delivery" != "null" ] && break
  sleep 1
done
[ "$(jq -r '.proposal.status' <<<"$read")" = "approved" ] || fail "the agent reads $(jq -r '.proposal.status' <<<"$read")"
[ "$delivery" = "done" ] || fail "the delivery is $delivery: $(jq -c '.proposal.delivery' <<<"$read")"
jq -e '.proposal.resultJson | fromjson | .reference' <<<"$read" >/dev/null || fail "no result from the receiver: $(jq -c '.proposal' <<<"$read")"

echo "smoke: back in with a sign-in link from the command line"
person_link="$("${compose[@]}" exec -T pgmandate /pgmandate sign-in-link -tenant "$tenant" -email sam@smoke.example | grep -o "$url/sign-in#[A-Za-z0-9_-]*")"
[ -n "$person_link" ] || fail "no sign-in link"
again="$(call /mandate.v1.AuthService/RedeemLink "" "$(jq -n --arg t "${person_link#*#}" '{token: $t}')" | jq -r '.token')"
name="$(call /mandate.v1.AuthService/Me "$again" '{}' | jq -r '.tenants[0].tenantName')"
[ "$name" = "Smoke Co" ] || fail "the sign-in link reached $name"

echo "smoke: ok"
