package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/testkit"
	"github.com/andreas-lindfalk/pgmandate/pkg/version"
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
	require.NoError(t, Demo(ctx, pg.AppUrl, "127.0.0.1:0", testfix.Signer, &out))
	text := out.String()
	t.Log(text)
	require.NotContains(t, text, "HOLE")
	require.Equal(t, 3, strings.Count(text, "allowed under a mandate, without a person"), "the printer, the keyboard and the monitor")
	require.Contains(t, text, "result: ", "the agent is shown what the helpdesk answered")
	require.Contains(t, text, "a repeat, nothing new was done", "the same request twice is done once")
	require.Contains(t, text, "go run ./cmd/pgmandate receiver -addr 127.0.0.1:")
	require.Regexp(t, `receiver -addr \S+ -tenant [0-9a-f-]{36} -audience pgm:[0-9a-f-]{36}:[a-z.]+:[0-9a-f]{16}`, text, "the receiver is told its tenant and audiences")
	require.Contains(t, text, "above what your mandate allows")
	require.Contains(t, text, "flags steering_text")
	require.Contains(t, text, "budget for this month is spent")
	require.Contains(t, text, "tickets.close → approved")
	require.Contains(t, text, "flagged ticket → rejected")
	require.Equal(t, 3, strings.Count(text, "signature, audience and tenant checked; decision executed"), "three executions and")
	require.Equal(t, 1, strings.Count(text, "signature, audience and tenant checked; decision approved"), "one approval are delivered")
	require.Contains(t, text, "after 2 attempt(s)", "the failed delivery was retried")
	require.Equal(t, 5, strings.Count(text, "refused: "), "five statements the database refuses")
	require.Contains(t, text, "trial-bot, dry_run")
	require.Contains(t, text, "dryRun true, would_allow", "a mandate would allow the dry run")
	require.Contains(t, text, "dryRun true, pending")
	require.Contains(t, text, "the helpdesk got 0 new deliveries, and the trial mandate has 0 of 3 executions used", "nothing of a dry run is delivered or spent")
	require.Contains(t, text, "tickets.close → dismissed")
	require.Contains(t, text, "created live (a new agent starts in dry run")
	require.Contains(t, text, "verified: ")
	require.Contains(t, text, `"outcome":"executed"`)

	// The command line works against the same database.
	t.Setenv("MANDATE_DATABASE_URL", pg.OwnerUrl)
	t.Setenv("MANDATE_APP_PASSWORD", "app")
	out.Reset()
	require.NoError(t, run(ctx, "tenant", []string{"create", "-name", "Cli Co", "-owner", "cli@test.example", "-owner-name", "Cli"}, &out))
	tenantId := submatch(t, `tenant ([0-9a-f-]{36})`, out.String())

	out.Reset()
	require.NoError(t, run(ctx, "agent", []string{"create", "-tenant", tenantId, "-as", "cli@test.example", "-name", "cli-bot"}, &out))
	require.Contains(t, out.String(), "token mnd_")
	require.Contains(t, out.String(), "in dry run: what it proposes is recorded and never carried out", "a new agent starts in dry run")
	agentId := submatch(t, `agent ([0-9a-f-]{36})`, out.String())
	agentToken := submatch(t, `token (mnd_\S+)`, out.String())

	out.Reset()
	require.NoError(t, run(ctx, "grant", []string{"-tenant", tenantId, "-as", "cli@test.example", "-agent", agentId, "-actions", "tickets.*", "-max-risk", "low", "-budget", "5", "-policy", "CLI-POLICY",
		"-condition", `action == "tickets.create"`}, &out))
	require.Contains(t, out.String(), "0 of 5 executions")
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
	require.NoError(t, run(ctx, "action", []string{"define", "-tenant", tenantId, "-as", "cli@test.example", "-key", "tickets.create", "-risk", "low", "-may-execute", "-url", "https://helpdesk.example/hooks", "-schema", `{"type":"object"}`}, &out))
	require.Regexp(t, `audience pgm:`+tenantId+`:tickets\.create:[0-9a-f]{16}`, out.String(), "the webhook's audience is printed for its receiver")
	require.NotContains(t, out.String(), "mws_", "there is no secret any more")
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
	// A dry run from the command line, dismissed; then the agent is made live.
	out.Reset()
	require.NoError(t, run(ctx, "propose", append(slices.Clone(proposeArgs[:len(proposeArgs)-2]), "-run", "dry-1"), &out))
	require.Contains(t, out.String(), `"dryRun": true`)
	dryId := submatch(t, `"proposalId": "([0-9a-f-]{36})"`, out.String())
	out.Reset()
	require.Error(t, run(ctx, "approve", []string{"-tenant", tenantId, "-as", "cli@test.example", "-proposal", dryId}, &out), "a dry run is never approved")
	require.NoError(t, run(ctx, "dismiss", []string{"-tenant", tenantId, "-as", "cli@test.example", "-proposal", dryId, "-note", "seen"}, &out))
	require.Contains(t, out.String(), "dismissed")
	require.Contains(t, out.String(), "(dry run: never carried out)")
	out.Reset()
	require.Error(t, run(ctx, "agent", []string{"mode", "-tenant", tenantId, "-as", "cli@test.example", "-agent", agentId, "-mode", "sometimes"}, &out))
	require.NoError(t, run(ctx, "agent", []string{"mode", "-tenant", tenantId, "-as", "cli@test.example", "-agent", agentId, "-mode", "live"}, &out))
	require.Contains(t, out.String(), "live: what it proposes is carried out")

	out.Reset()
	require.NoError(t, run(ctx, "propose", proposeArgs, &out))
	require.Contains(t, out.String(), `"created": true`)
	require.Contains(t, out.String(), `"dryRun": false`)
	// The command line names itself and its release in the provenance the
	// log keeps.
	shellId := submatch(t, `"proposalId": "([0-9a-f-]{36})"`, out.String())
	var client, clientVersion string
	require.NoError(t, pg.Owner.QueryRow(ctx, `SELECT provenance->>'client', provenance->>'clientVersion' FROM mandate.proposals WHERE id = $1`, shellId).Scan(&client, &clientVersion))
	require.Equal(t, "pgmandate-cli", client)
	require.Equal(t, version.String(), clientVersion)
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
	require.NoError(t, pg.Owner.QueryRow(ctx, `SELECT count(*) FROM mandate_river.river_job WHERE kind = 'mandate.deliver' AND state = 'available'`).Scan(&queued))
	require.Equal(t, 1, queued, "the delivery job waits for a server")

	require.Error(t, run(ctx, "nonsense", nil, &out))
	for _, command := range []string{"version", "--version"} {
		out.Reset()
		require.NoError(t, run(ctx, command, nil, &out))
		require.Equal(t, "pgmandate "+version.String()+"\n", out.String())
	}
	require.Error(t, run(ctx, "-v", nil, &out), "-v stays free for verbosity")
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

// The receiver trusts the keys it is given, or the set the server
// publishes, and refuses to start trusting nothing.
func TestReceiverKeys(t *testing.T) {
	previous := newTestKey(t, 7)
	published := httptest.NewServer(keysHandler(testfix.Signer, []ed25519.PublicKey{previous}))
	defer published.Close()
	keys, err := receiverKeys(context.Background(), "", published.URL)
	require.NoError(t, err)
	require.Len(t, keys, 2, "the signing key and the previous one")
	require.Equal(t, testfix.Signer.PublicKey(), keys[testfix.Signer.KeyId()])

	given, err := receiverKeys(context.Background(), base64.StdEncoding.EncodeToString(previous), "http://unused.invalid")
	require.NoError(t, err)
	require.Len(t, given, 1)

	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	_, err = receiverKeys(context.Background(), "", missing.URL)
	require.ErrorContains(t, err, "-keys", "a server without the set is not read as one with no keys")
	_, err = receiverKeys(context.Background(), "not base64", "")
	require.Error(t, err)
}

func newTestKey(t *testing.T, fill byte) ed25519.PublicKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill + byte(i)
	}
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

// submatch is the first group of pattern in text, or a test failure that
// shows the text.
func submatch(t *testing.T, pattern, text string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(text)
	require.Len(t, m, 2, "no %s in:\n%s", pattern, text)
	return m[1]
}

// The version and the help answer even when the environment is wrong,
// which is when an operator asks.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	t.Setenv("MANDATE_SESSION_TTL", "not a duration")
	var out bytes.Buffer
	require.Error(t, run(context.Background(), "migrate", nil, &out), "the configuration is wrong")
	require.NoError(t, run(context.Background(), "version", nil, &out))
	require.Equal(t, "pgmandate "+version.String()+"\n", out.String())
	out.Reset()
	require.NoError(t, run(context.Background(), "help", nil, &out))
	require.Contains(t, out.String(), "pgmandate version")
}

// A release build reports the version it was linked with: the path the
// Dockerfile and a release use is the one the binary reads.
func TestALinkedVersionIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	binary := filepath.Join(t.TempDir(), "pgmandate")
	build := exec.Command("go", "build", "-ldflags", "-X 'github.com/andreas-lindfalk/pgmandate/pkg/version.Version=v9.8.7-test'", "-o", binary, ".")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	printed, err := exec.Command(binary, "version").Output()
	require.NoError(t, err)
	require.Equal(t, "pgmandate v9.8.7-test\n", string(printed))
}

// The command line sets pgmandate up as a managed Postgres's admin, who is
// no superuser, and refuses to set up or serve over an app role that could
// get past the rules.
func TestSetupAsAManagedAdmin(t *testing.T) {
	ctx := context.Background()
	pg, superuser, err := testkit.StartBarePostgres(ctx)
	require.NoError(t, err)
	defer pg.Close()
	conn, err := pgx.Connect(ctx, superuser)
	require.NoError(t, err)
	defer conn.Close(ctx) //nolint:errcheck
	for _, statement := range []string{
		`CREATE ROLE cloud_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER PASSWORD 'admin'`,
		`CREATE DATABASE managed OWNER cloud_admin`,
	} {
		_, err := conn.Exec(ctx, statement)
		require.NoError(t, err)
	}
	parsed, err := url.Parse(superuser)
	require.NoError(t, err)
	parsed.User, parsed.Path = url.UserPassword("cloud_admin", "admin"), "/managed"
	t.Setenv("MANDATE_DATABASE_URL", parsed.String())
	t.Setenv("MANDATE_APP_PASSWORD", "app-secret")

	var out bytes.Buffer
	t.Setenv("MANDATE_CREATE_APP_ROLE", "no")
	require.ErrorContains(t, run(ctx, "migrate", nil, &out), "MANDATE_CREATE_APP_ROLE is true or false", "a word that is not a boolean is refused, not read as yes")
	t.Setenv("MANDATE_CREATE_APP_ROLE", "true")
	require.NoError(t, run(ctx, "migrate", nil, &out))
	require.NoError(t, run(ctx, "tenant", []string{"create", "-name", "Managed Co", "-owner", "a@managed.example", "-owner-name", "A"}, &out))
	require.Contains(t, out.String(), "tenant ")
	cfg, err := loadConfig()
	require.NoError(t, err)
	pool, err := openServingApp(ctx, cfg)
	require.NoError(t, err, "the server's own check passes")
	pool.Close()

	// A DBA later lets the app role bypass row level security: setup and
	// the server both refuse.
	_, err = conn.Exec(ctx, `ALTER ROLE mandate_app BYPASSRLS`)
	require.NoError(t, err)
	require.ErrorIs(t, run(ctx, "migrate", nil, &out), db.ErrAppRole)
	_, err = openServingApp(ctx, cfg)
	require.ErrorIs(t, err, db.ErrAppRole)
	require.ErrorContains(t, err, "bypasses row level security")

	// Put right, and with a role the DBA keeps: setup leaves it alone.
	_, err = conn.Exec(ctx, `ALTER ROLE mandate_app NOBYPASSRLS`)
	require.NoError(t, err)
	t.Setenv("MANDATE_CREATE_APP_ROLE", "false")
	t.Setenv("MANDATE_APP_PASSWORD", "")
	require.NoError(t, run(ctx, "migrate", nil, &out))
	app, err := db.Open(ctx, func() string { u := *parsed; u.User = url.UserPassword("mandate_app", "app-secret"); return u.String() }())
	require.NoError(t, err, "the password stays what it was")
	app.Close()
}
