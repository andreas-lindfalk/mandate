# The README's terminal film: an agent proposes over MCP, and the decision
# arrives at the receiver as a signed webhook. Played by VHS
# (https://github.com/charmbracelet/vhs) in tmux; `make demo-terminal-gif`
# records it again into docs/terminal.gif. It needs vhs, tmux, and the
# local Postgres (`make up`), where it adds a tenant, and port 8585 free.
#
# Nothing is staged: docs/film/setup.sh starts a real server and sets up a
# tenant before recording starts, and what is shown runs as typed.

Output docs/terminal.gif

Require tmux

Set Shell "bash"
Set FontSize 16
Set Width 1000
Set Height 560
Set Padding 12
Set TypingSpeed 35ms
Set Framerate 10

Hide
Type "go build -o .local/bin/pgmandate ./cmd/pgmandate"
Enter
Wait@300s
Type "source docs/film/setup.sh && film_panes"
Enter
Sleep 15s
Show

Sleep 2s
Type "# A mandate lets this agent open tickets, up to 20 a month."
Enter
Sleep 3s
Type `pgmandate propose -brief -action tickets.create -title "Printer jams" -rationale "Three reports." -payload '{"subject":"Printer jams"}'`
Enter
Sleep 7s

Type "# A refund is high risk: no mandate allows it."
Enter
Sleep 3s
Type `pgmandate propose -brief -action refunds.issue -title "Refund order 1042" -rationale "Arrived broken." -payload '{"order":"1042","amount":40}'`
Enter
Sleep 6s
Type "# It waits in the inbox for a person. Nothing reached the helpdesk."
Enter
Sleep 7s

# Out of the film's tmux, then the server stops.
Hide
Type "tmux -L pgmandate-film kill-server"
Enter
Sleep 1s
Type "film_stop"
Enter
