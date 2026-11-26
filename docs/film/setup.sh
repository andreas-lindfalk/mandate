# Sourced by docs/film/terminal.tape before recording starts: builds the
# command line, starts a server, and sets up a tenant with one live agent,
# two webhook actions and one mandate, on the Postgres that
# MANDATE_DATABASE_URL names (the local one by default). Then film_panes
# opens the two panes the film shows, and film_stop ends it all.

go build -o .local/bin/pgmandate ./cmd/pgmandate || return
if curl -sf http://127.0.0.1:8585/healthz >/dev/null; then
  echo "port 8585 is taken: stop the server running there (make run?) first" >&2
  return 1
fi
export PATH="$PWD/.local/bin:$PATH" PS1='$ ' LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
pgmandate migrate >/dev/null || return
MANDATE_ALLOW_PRIVATE_CONNECTORS=true pgmandate serve >.local/film-serve.log 2>&1 &
FILM_SERVER=$!
until curl -sf http://127.0.0.1:8585/healthz >/dev/null; do
  if ! kill -0 "$FILM_SERVER" 2>/dev/null; then
    echo "the server stopped; see .local/film-serve.log" >&2
    return 1
  fi
  sleep 0.3
done

# An owner of its own, so that the demo's alice stays in one tenant.
as=(-as film@acme.example)
FILM_TENANT=$(pgmandate tenant create -name "Acme Support" -owner film@acme.example -owner-name Fran | awk '/^tenant/{print $2}')
FILM_TICKETS=$(pgmandate action define -tenant "$FILM_TENANT" "${as[@]}" -key tickets.create -risk low -may-execute -url http://127.0.0.1:8586/ \
  -schema '{"type":"object","required":["subject"],"properties":{"subject":{"type":"string"}}}' | awk '/^audience/{print $2}')
FILM_REFUNDS=$(pgmandate action define -tenant "$FILM_TENANT" "${as[@]}" -key refunds.issue -risk high -url http://127.0.0.1:8586/ \
  -schema '{"type":"object","required":["order","amount"],"properties":{"order":{"type":"string"},"amount":{"type":"number"}}}' | awk '/^audience/{print $2}')
agent=$(pgmandate agent create -tenant "$FILM_TENANT" "${as[@]}" -name support-bot -mode live)
export MANDATE_AGENT_TOKEN=$(awk '/^token/{print $2}' <<<"$agent")
pgmandate grant -tenant "$FILM_TENANT" "${as[@]}" -agent "$(awk '/^agent/{print $2}' <<<"$agent")" \
  -actions 'tickets.*' -max-risk low -budget 20 -policy "AI policy §4" >/dev/null

# The receiver above, next to the helpdesk; the agent below. A tmux server
# of the film's own, with no configuration read, so that nothing of yours
# is touched or changes it; both panes run a bare bash, so that no profile
# changes the prompt or the titles.
film_tmux() { tmux -L pgmandate-film -f /dev/null "$@"; }

film_panes() {
  local shell="bash --norc --noprofile"
  film_tmux new-session -d -s film "$shell"
  film_tmux set -g status off \; set -g pane-border-status top \; set -g pane-border-format " #{pane_title} " \; set -g allow-set-title off
  film_tmux split-window -v -b -l 7 -t film "$shell"
  film_tmux select-pane -t film:0.0 -T "The receiver, next to the helpdesk: it holds the helpdesk's credential"
  film_tmux send-keys -t film:0.0 "clear; pgmandate receiver -brief -tenant $FILM_TENANT -audience $FILM_TICKETS,$FILM_REFUNDS" Enter
  film_tmux select-pane -t film:0.1 -T "The agent: it has no credential, and proposes over MCP"
  film_tmux select-pane -t film:0.1
  film_tmux send-keys -t film:0.1 "clear" Enter
  film_tmux attach -t film
}

film_stop() {
  film_tmux kill-server 2>/dev/null
  kill "$FILM_SERVER" 2>/dev/null
}
