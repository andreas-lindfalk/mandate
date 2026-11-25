package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
)

// demoTenant finds the tenant id the demo printed.
func demoTenant(t *testing.T, text string) string {
	t.Helper()
	m := regexp.MustCompile(`tenant   ([0-9a-f-]{36})`).FindStringSubmatch(text)
	require.NotNil(t, m)
	return m[1]
}

func TestTheServerAnswersOnlyToThisMachineWhenDevLoginIsOn(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	get := func(cfg config, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mandate.v1.AuthService/DevLogin", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		harden(next, cfg).ServeHTTP(rec, req)
		return rec
	}
	dev := config{devLogin: true, publicUrl: "http://127.0.0.1:8585"}
	require.Equal(t, http.StatusNoContent, get(dev, "127.0.0.1:8585").Code)
	require.Equal(t, http.StatusNoContent, get(dev, "localhost:8585").Code)
	require.Equal(t, http.StatusNoContent, get(dev, "[::1]:8585").Code)
	require.Equal(t, http.StatusForbidden, get(dev, "evil.example:8585").Code, "a rebound name does not reach the dev login")
	require.Equal(t, http.StatusForbidden, get(dev, "mandate.example").Code, "nor does a reverse proxy in front of it")

	hosted := config{publicUrl: "https://mandate.example"}
	rec := get(hosted, "mandate.example")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'")
	require.NotEmpty(t, rec.Header().Get("Strict-Transport-Security"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Empty(t, get(dev, "localhost").Header().Get("Strict-Transport-Security"))

	require.True(t, loopback("127.0.0.1:8585"))
	require.True(t, loopback("localhost:8585"))
	require.False(t, loopback("0.0.0.0:8585"))
	require.False(t, loopback(":8585"))
}

func bytesOf(path string, t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// The demo is the product's front door: it must run end to end, show every
// refusal, and never find a hole.
func TestDemoAndCommandLine(t *testing.T) {
	ctx := context.Background()
	pg, err := testkit.StartPostgres(ctx)
	require.NoError(t, err)
	defer pg.Close()

	var out bytes.Buffer
	require.NoError(t, Demo(ctx, pg.AppUrl, "127.0.0.1:0", &out))
	text := out.String()
	t.Log(text)
	require.NotContains(t, text, "HOLE")
	require.Equal(t, 3, strings.Count(text, "allowed under a mandate, without a person"), "the printer, the keyboard and the monitor")
	require.Contains(t, text, "result: ", "the agent is shown what the helpdesk answered")
	require.Contains(t, text, "a repeat, nothing new was done", "the same request twice is done once")
	require.Contains(t, text, "go run ./cmd/pgmandate receiver -addr 127.0.0.1:")
	require.Contains(t, text, "above what your mandate allows")
	require.Contains(t, text, "flags steering_text")
	require.Contains(t, text, "budget for this month is spent")
	require.Contains(t, text, "tickets.close → approved")
	require.Contains(t, text, "flagged ticket → rejected")
	require.Equal(t, 3, strings.Count(text, "signature verified; decision executed"), "three executions and")
	require.Equal(t, 1, strings.Count(text, "signature verified; decision approved"), "one approval are delivered")
	require.Contains(t, text, "after 2 attempt(s)", "the failed delivery was retried")
	require.Equal(t, 5, strings.Count(text, "refused: "), "five statements the database refuses")
	require.Contains(t, text, "verified: ")
	require.Contains(t, text, `"outcome":"executed"`)

	// The command line works against the same database.
	t.Setenv("MANDATE_DATABASE_URL", pg.OwnerUrl)
	t.Setenv("MANDATE_APP_PASSWORD", "app")
	out.Reset()
	require.NoError(t, run(ctx, "tenant", []string{"create", "-name", "Cli Co", "-owner", "cli@test.example", "-owner-name", "Cli"}, &out))
	tenantId := regexp.MustCompile(`tenant ([0-9a-f-]{36})`).FindStringSubmatch(out.String())[1]

	out.Reset()
	require.NoError(t, run(ctx, "agent", []string{"create", "-tenant", tenantId, "-as", "cli@test.example", "-name", "cli-bot"}, &out))
	require.Contains(t, out.String(), "token mnd_")
	agentId := regexp.MustCompile(`agent ([0-9a-f-]{36})`).FindStringSubmatch(out.String())[1]
	agentToken := regexp.MustCompile(`token (mnd_\S+)`).FindStringSubmatch(out.String())[1]

	out.Reset()
	require.NoError(t, run(ctx, "grant", []string{"-tenant", tenantId, "-as", "cli@test.example", "-agent", agentId, "-actions", "tickets.*", "-max-risk", "low", "-budget", "5", "-policy", "CLI-POLICY",
		"-condition", `action == "tickets.create"`}, &out))
	require.Contains(t, out.String(), "0 of 5 units")
	require.Contains(t, out.String(), `when action == "tickets.create"`)
	require.Error(t, run(ctx, "grant", []string{"-tenant", tenantId, "-as", "cli@test.example", "-actions", "*", "-max-risk", "high", "-budget", "5", "-policy", "P"}, &out))

	out.Reset()
	require.NoError(t, run(ctx, "mandates", []string{"-tenant", tenantId}, &out))
	require.Contains(t, out.String(), "CLI-POLICY")
	require.Contains(t, out.String(), `when action == "tickets.create"`)
	out.Reset()
	require.NoError(t, run(ctx, "verify", []string{"-tenant", tenantId}, &out))
	require.Contains(t, out.String(), "chain intact: 5 events")
	out.Reset()
	require.NoError(t, run(ctx, "events", []string{"-tenant", tenantId}, &out))
	require.Contains(t, out.String(), "mandate.granted")

	out.Reset()
	require.NoError(t, run(ctx, "action", []string{"define", "-tenant", tenantId, "-as", "cli@test.example", "-key", "tickets.create", "-risk", "low", "-cost", "1", "-may-execute", "-url", "https://helpdesk.example/hooks", "-schema", `{"type":"object"}`}, &out))
	require.Contains(t, out.String(), "secret mws_")
	out.Reset()
	require.NoError(t, run(ctx, "actions", []string{"-tenant", tenantId}, &out))
	require.Contains(t, out.String(), "action tickets.create")
	require.Contains(t, out.String(), "may execute under a mandate")
	require.Error(t, run(ctx, "action", []string{"define", "-tenant", tenantId, "-as", "cli@test.example", "-key", "Bad.Key", "-url", "https://x"}, &out))

	out.Reset()
	require.NoError(t, run(ctx, "export-chain", []string{"-tenant", tenantId}, &out))
	chainFile := filepath.Join(t.TempDir(), "chain.jsonl")
	require.NoError(t, os.WriteFile(chainFile, out.Bytes(), 0o600))
	out.Reset()
	require.NoError(t, run(ctx, "verify-export", []string{"-tenant", tenantId, "-file", chainFile}, &out))
	require.Contains(t, out.String(), "chain intact: 6 events, head ")
	tampered := strings.Replace(string(bytesOf(chainFile, t)), `"mandate.granted"`, `"mandate.revoked"`, 1)
	require.NoError(t, os.WriteFile(chainFile, []byte(tampered), 0o600))
	require.ErrorContains(t, run(ctx, "verify-export", []string{"-tenant", tenantId, "-file", chainFile}, &out), "broken at seq 5")
	require.Error(t, run(ctx, "verify-export", []string{"-tenant", agentId, "-file", chainFile}, &out), "another tenant's id does not verify it")

	// The agent proposes from the command line, over MCP, as any agent does.
	appPool, err := db.Open(ctx, pg.AppUrl)
	require.NoError(t, err)
	defer appPool.Close()
	mcpServer := httptest.NewServer(pgmandate.New(appPool, pgmandate.Options{}).Handler())
	defer mcpServer.Close()
	proposeArgs := []string{"-token", agentToken, "-url", mcpServer.URL + "/mcp", "-action", "tickets.create", "-title", "From the shell", "-rationale", "Tried from a shell.",
		"-payload", `{"subject":"Printer","account":9007199254740993}`, "-run", "shell-1"}
	out.Reset()
	require.NoError(t, run(ctx, "propose", proposeArgs, &out))
	require.Contains(t, out.String(), `"created": true`)
	require.Contains(t, out.String(), "9007199254740993", "the payload goes as written")
	require.Contains(t, out.String(), "run shell-1, key shell-1:1")
	out.Reset()
	require.NoError(t, run(ctx, "propose", proposeArgs, &out))
	require.Contains(t, out.String(), `"repeat": true`, "the same run and key again is a repeat")
	require.Error(t, run(ctx, "propose", []string{"-token", "mnd_nope", "-url", mcpServer.URL + "/mcp", "-action", "tickets.create", "-title", "x", "-rationale", "r"}, &out))
	require.Error(t, run(ctx, "propose", []string{"-action", "tickets.create"}, &out), "a token is needed")
	out.Reset()
	require.ErrorContains(t, run(ctx, "propose", []string{"-token", agentToken, "-url", mcpServer.URL + "/mcp", "-action", "tickets.create", "-title", "x", "-payload", "[1]", "-rationale", "r"}, &out), "-payload must be a JSON object")
	out.Reset()
	require.NoError(t, run(ctx, "propose", []string{"-token", agentToken, "-url", mcpServer.URL + "/mcp", "-action", "tickets.create", "-title", "a\x01b \"quoted\"", "-rationale", "Quotes \"and\" control\x01 characters.", "-payload", `{"subject":"x"}`}, &out), "a title with control characters and quotes is sent as JSON")
	require.Contains(t, out.String(), `"created": true`)

	// An approval from the command line queues its delivery for the server.
	out.Reset()
	require.NoError(t, run(ctx, "proposals", []string{"-tenant", demoTenant(t, text), "-status", "pending"}, &out))
	refund := regexp.MustCompile(`([0-9a-f-]{36}) pending   high    refunds.issue`).FindStringSubmatch(out.String())
	require.NotNil(t, refund, out.String())
	out.Reset()
	require.NoError(t, run(ctx, "approve", []string{"-tenant", demoTenant(t, text), "-as", "alice@acme.example", "-proposal", refund[1], "-confirm"}, &out))
	require.Contains(t, out.String(), "approved")
	require.Contains(t, out.String(), "delivery pending")
	var queued int
	require.NoError(t, pg.Owner.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'mandate.deliver' AND state = 'available'`).Scan(&queued))
	require.Equal(t, 1, queued, "the delivery job waits for a server")

	require.Error(t, run(ctx, "nonsense", nil, &out))
	require.Error(t, run(ctx, "approve", []string{"-tenant", tenantId, "-as", "nobody@test.example", "-proposal", agentId}, &out))

	// Notifications are set and turned off from the command line too.
	out.Reset()
	require.NoError(t, run(ctx, "notify", []string{"set", "-tenant", tenantId, "-as", "cli@test.example", "-kind", "discord", "-url", "https://discord.com/api/webhooks/1/abc"}, &out))
	require.Contains(t, out.String(), "notifications go to a discord channel")
	// The address from standard input, kept out of the shell's history.
	stdin = strings.NewReader("https://hooks.slack.com/services/T0/B0/secret\n")
	defer func() { stdin = os.Stdin }()
	out.Reset()
	require.NoError(t, run(ctx, "notify", []string{"set", "-tenant", tenantId, "-as", "cli@test.example", "-kind", "slack"}, &out))
	require.Contains(t, out.String(), "notifications go to a slack channel")
	require.NotContains(t, out.String(), "secret")
	stdin = strings.NewReader("")
	require.ErrorContains(t, run(ctx, "notify", []string{"set", "-tenant", tenantId, "-as", "cli@test.example", "-kind", "slack"}, &out), "standard input")
	require.ErrorContains(t, run(ctx, "notify", []string{"set", "-tenant", tenantId, "-as", "cli@test.example", "-kind", "slack", "-url", "https://example.com/hook"}, &out), "incoming webhook")
	out.Reset()
	require.NoError(t, run(ctx, "notify", []string{"remove", "-tenant", tenantId, "-as", "cli@test.example"}, &out))
	require.Contains(t, out.String(), "notifications are off")
	require.Error(t, run(ctx, "notify", []string{"remove", "-tenant", tenantId, "-as", "cli@test.example"}, &out), "nothing left to remove")
}

func TestDescribeDeliveryShowsTheIdsAndTheBody(t *testing.T) {
	deliveryId := uuid.New()
	body := []byte(`{"version":1,"deliveryId":"` + deliveryId.String() + `","correlationId":"run-7","proposal":{"action":"tickets.create","title":"Printer"},"decision":{"status":"approved"}}`)
	var payload webhook.Payload
	require.NoError(t, json.Unmarshal(body, &payload))
	text := describeDelivery(webhook.Received{DeliveryId: deliveryId, Payload: payload, Body: body, At: time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)})
	require.Contains(t, text, "09:30:00  approved  tickets.create")
	require.Contains(t, text, "delivery "+deliveryId.String())
	require.Contains(t, text, "correlation run-7")
	require.Contains(t, text, `"version": 1`, "the body is printed as it arrived")
	require.Contains(t, text, `"correlationId": "run-7"`)

	// A body that is not JSON is printed as it is.
	text = describeDelivery(webhook.Received{DeliveryId: deliveryId, Body: []byte("not json"), At: time.Now()})
	require.Contains(t, text, "not json")
}

func TestTheDefaultAppPasswordStaysOnThisMachine(t *testing.T) {
	require.True(t, usesDefaultAppPassword(config{appPassword: "app"}))
	require.False(t, usesDefaultAppPassword(config{appPassword: "s3cret"}))
	require.True(t, usesDefaultAppPassword(config{appUrl: "postgres://mandate_app:app@db:5432/mandate"}))
	require.False(t, usesDefaultAppPassword(config{appUrl: "postgres://mandate_app:s3cret@db:5432/mandate", appPassword: "app"}))
	require.True(t, usesDefaultAppPassword(config{appUrl: "host=db user=mandate_app password=app dbname=mandate"}))
	require.True(t, usesDefaultAppPassword(config{appUrl: "postgres://mandate_app@db:5432/mandate?password=app"}))
	require.True(t, usesDefaultAppPassword(config{appUrl: "postgres://%zz"}), "a connection string that cannot be read is refused")

	err := serve(context.Background(), config{appUrl: "postgres://mandate_app:app@127.0.0.1:1/mandate", httpAddr: "0.0.0.0:8585"})
	require.ErrorContains(t, err, "default app password is refused")
}
