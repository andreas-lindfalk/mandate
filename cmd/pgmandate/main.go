// Command pgmandate is the service: it serves the UI and the API to people,
// MCP to agents and deliveries to connectors, applies the schema, and lets
// an operator run a tenant from the command line. Actions are defined as data; a host that embeds the
// library instead registers builtin actions in its own binary.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andreas-lindfalk/pgmandate"
	"github.com/andreas-lindfalk/pgmandate/api"
	"github.com/andreas-lindfalk/pgmandate/examples/webhook"
	"github.com/andreas-lindfalk/pgmandate/modules/actions"
	"github.com/andreas-lindfalk/pgmandate/modules/agents"
	"github.com/andreas-lindfalk/pgmandate/modules/auth"
	"github.com/andreas-lindfalk/pgmandate/modules/deliveries"
	"github.com/andreas-lindfalk/pgmandate/modules/mandates"
	"github.com/andreas-lindfalk/pgmandate/modules/proposals"
	"github.com/andreas-lindfalk/pgmandate/modules/tenants"
	"github.com/andreas-lindfalk/pgmandate/pkg/actor"
	"github.com/andreas-lindfalk/pgmandate/pkg/db"
	"github.com/andreas-lindfalk/pgmandate/pkg/events"
	"github.com/andreas-lindfalk/pgmandate/pkg/jobs"
	"github.com/andreas-lindfalk/pgmandate/pkg/version"
	"github.com/andreas-lindfalk/pgmandate/schema"
	"github.com/andreas-lindfalk/pgmandate/sign"
	"github.com/andreas-lindfalk/pgmandate/web"
)

const usage = `pgmandate: agents propose, a person or a mandate decides, Postgres checks again.

  pgmandate migrate                  create the app role and apply the schema (needs the owner connection)
  pgmandate serve                    migrate when the owner connection is given, then serve the UI, the API
                                     and the agent contract over MCP (/mcp) and HTTP on $MANDATE_HTTP_ADDR
  pgmandate demo                     the five-minute demo (needs a running Postgres)
  pgmandate version                  the release this binary was built from
  pgmandate receiver -tenant ID -audience A1,A2 [-keys K1,K2 | -keys-url URL] [-addr 127.0.0.1:8586]
                                     a local receiver that prints the deliveries it accepts; it trusts the
                                     server's published keys unless -keys names them
  pgmandate propose -token mnd_... -action a.b -title T -rationale WHY -payload '{...}' [-evidence '[...]'] [-key K] [-run R] [-url U]
                                     propose as an agent over MCP, as any agent would, and print the answer

The commands below act on the database directly, as the person named with -as. Run migrate or serve first.

  pgmandate tenant create -name NAME -owner EMAIL -owner-name NAME
  pgmandate people -tenant ID
  pgmandate person add -tenant ID -as EMAIL -email EMAIL -name NAME [-role owner|reviewer]
  pgmandate person remove -tenant ID -as EMAIL -person ID
  pgmandate agent create -tenant ID -as EMAIL -name NAME [-mode dry_run|live] [-department D] [-days 90]
                          prints the token once. A new agent is in dry run unless -mode live: what it
                          proposes is recorded, never carried out, while it acts with its own credential.
  pgmandate agent mode -tenant ID -as EMAIL -agent ID -mode live|dry_run
  pgmandate agent revoke -tenant ID -as EMAIL -agent ID
  pgmandate action define -tenant ID -as EMAIL -key a.b [-risk low|medium|high] [-may-execute]
                          [-connector webhook|builtin] [-url http(s)://...] [-schema '{...}' | -schema-file f.json]
                          [-wait 168h] [-deliver-within 1h]
                          [-description TEXT] [-department D]                    prints the webhook's audience
                          A webhook needs -url and a schema. -wait is how long a proposal waits for a decision
                          before it expires (1m to 720h); -deliver-within how long after a decision its delivery
                          may be carried out (2m to 168h). -may-execute lets a mandate allow it without a person;
                          without it, a person always decides. Changing -url gives a new audience.
  pgmandate action remove -tenant ID -as EMAIL -key a.b
  pgmandate actions -tenant ID [-all]
  pgmandate grant -tenant ID -as EMAIL [-agent ID] -actions a.b,c.* [-max-risk low|medium] -budget N -policy REF
                  [-days 90] [-department D] [-condition 'payload.amount <= 500']
                  -budget is how many executions it may allow per month. Without -agent the mandate
                  covers every agent of the tenant. -condition narrows it by the
                  payload: fields compared with literals (== != < <= > >= in), combined with && || !.
  pgmandate agent limits -tenant ID -as EMAIL -agent ID -per-minute 30 -per-hour 300
                  How many proposals the agent may make; the database refuses the rest.
  pgmandate notify set -tenant ID -as EMAIL -kind slack|discord < address-file   (or -url https://hooks.slack.com/...)
  pgmandate notify test -tenant ID -as EMAIL
                  A message in that channel when a proposal waits for a person: the action, its risk, the
                  agent, why it waits and a link, nothing the agent wrote. notify remove turns it off.
  pgmandate revoke -tenant ID -as EMAIL -mandate ID
  pgmandate mandates -tenant ID [-all]
  pgmandate proposals -tenant ID [-status pending|approved|rejected|executed|withdrawn|expired|would_allow|dismissed] [-limit 50]
  pgmandate approve -tenant ID -as EMAIL -proposal ID [-note TEXT] [-confirm]
  pgmandate reject -tenant ID -as EMAIL -proposal ID [-note TEXT]
  pgmandate dismiss -tenant ID -as EMAIL -proposal ID [-note TEXT]   set a waiting dry run aside
  pgmandate redeliver -tenant ID -as EMAIL -proposal ID        send again a delivery that was never sent
  pgmandate settle -tenant ID -as EMAIL -proposal ID -done true|false
                                                               say whether a delivery whose outcome is unknown was done
  pgmandate events -tenant ID [-subject ID] [-limit 50]
  pgmandate verify -tenant ID                                  recompute the log's hash chain
  pgmandate export -tenant ID                                  decided proposals as JSON lines (an eval set)
  pgmandate export-chain -tenant ID                            the whole chain as JSON lines, hashes included
  pgmandate verify-export -tenant ID -file chain.jsonl         recompute an exported chain without the database

Environment:
  MANDATE_DATABASE_URL       the owner connection, for migrate (default: the local Postgres on :5433)
  MANDATE_APP_PASSWORD       the password of the mandate_app role (default app, refused beyond loopback)
  MANDATE_APP_DATABASE_URL   connect as the app role directly; with it and without MANDATE_DATABASE_URL,
                               serve does not migrate and never holds the owner's credentials
  MANDATE_HTTP_ADDR          default 127.0.0.1:8585
  MANDATE_PUBLIC_URL         default http://<addr>
  MANDATE_DEV_LOGIN          true lets anyone sign in with an email; only on a loopback address
  MANDATE_SESSION_TTL        default 168h
  MANDATE_OIDC_ISSUER, MANDATE_OIDC_CLIENT_ID, MANDATE_OIDC_CLIENT_SECRET, MANDATE_OIDC_REDIRECT_URL
  MANDATE_ALLOW_PRIVATE_CONNECTORS   true lets webhook connectors live on local and private addresses
  MANDATE_RECEIVER_ADDR      where receiver and the demo's receiver listen, default 127.0.0.1:8586
  MANDATE_AGENT_TOKEN        the token propose uses when -token is not given
  MANDATE_SIGNING_KEY        the Ed25519 seed deliveries are signed with, 32 bytes in standard base64
  MANDATE_SIGNING_KEY_FILE   the same, in a file. Without either, on a loopback address, one is made
                               in .local/signing-key; beyond loopback, serve refuses to start
  MANDATE_SIGNING_KEYS_PREVIOUS  comma separated public keys still published at
                               /.well-known/pgmandate-keys, after a rotation
`

type config struct {
	ownerUrl    string
	appPassword string
	httpAddr    string
	// appUrl, when set, is how the server connects as the app role; the
	// owner connection is then not needed to serve.
	appUrl     string
	ownerGiven bool
	// allowPrivate lets webhook connectors live on local and private addresses.
	allowPrivate bool
	receiverAddr string
	publicUrl    string
	devLogin     bool
	sessionTtl   time.Duration
	oidc         *auth.OidcConfig
}

func loadConfig() (config, error) {
	cfg := config{
		ownerUrl:     env("MANDATE_DATABASE_URL", "postgres://mandate_owner:owner@localhost:5433/mandate?sslmode=disable"),
		appPassword:  env("MANDATE_APP_PASSWORD", "app"),
		httpAddr:     env("MANDATE_HTTP_ADDR", "127.0.0.1:8585"),
		devLogin:     env("MANDATE_DEV_LOGIN", "false") == "true",
		appUrl:       os.Getenv("MANDATE_APP_DATABASE_URL"),
		ownerGiven:   os.Getenv("MANDATE_DATABASE_URL") != "",
		allowPrivate: env("MANDATE_ALLOW_PRIVATE_CONNECTORS", "false") == "true",
		receiverAddr: env("MANDATE_RECEIVER_ADDR", "127.0.0.1:8586"),
	}
	cfg.publicUrl = env("MANDATE_PUBLIC_URL", "http://"+cfg.httpAddr)
	ttl, err := time.ParseDuration(env("MANDATE_SESSION_TTL", "168h"))
	if err != nil {
		return cfg, fmt.Errorf("MANDATE_SESSION_TTL: %w", err)
	}
	cfg.sessionTtl = ttl
	if issuer := os.Getenv("MANDATE_OIDC_ISSUER"); issuer != "" {
		cfg.oidc = &auth.OidcConfig{
			Issuer: issuer, ClientId: os.Getenv("MANDATE_OIDC_CLIENT_ID"), ClientSecret: os.Getenv("MANDATE_OIDC_CLIENT_SECRET"),
			RedirectUrl: os.Getenv("MANDATE_OIDC_REDIRECT_URL"),
		}
		if cfg.oidc.ClientId == "" {
			return cfg, errors.New("MANDATE_OIDC_CLIENT_ID is required with MANDATE_OIDC_ISSUER")
		}
	}
	return cfg, nil
}

// loopback says whether an address listens on this machine only, which is
// the only place dev login is allowed.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// stdin is where commands read a secret from, so tests can give one.
var stdin io.Reader = os.Stdin

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, command string, args []string, out io.Writer) error {
	// The version and the help need no configuration, so they answer even
	// when the environment is wrong: that is when an operator asks.
	switch command {
	case "version", "--version":
		fmt.Fprintf(out, "pgmandate %s\n", version.String())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch command {
	case "migrate":
		return migrate(ctx, cfg)
	case "serve":
		return serve(ctx, cfg)
	case "demo":
		if err := migrate(ctx, cfg); err != nil {
			return err
		}
		appUrl, err := db.AppUrl(cfg.ownerUrl, cfg.appPassword)
		if err != nil {
			return err
		}
		signer, _, err := signingKeys(cfg)
		if err != nil {
			return err
		}
		return Demo(ctx, appUrl, cfg.receiverAddr, signer, out)
	case "receiver":
		return receiver(ctx, cfg, args, out)
	case "propose":
		return proposeCommand(ctx, cfg, args, out)
	case "tenant", "person", "agent", "action", "notify":
		if len(args) == 0 {
			return fmt.Errorf("%s needs a subcommand", command)
		}
		return admin(ctx, cfg, command+" "+args[0], args[1:], out)
	case "grant", "revoke", "mandates", "proposals", "approve", "reject", "dismiss", "redeliver", "settle", "people", "events", "verify", "export", "export-chain", "actions":
		return admin(ctx, cfg, command, args, out)
	case "verify-export":
		return verifyExport(args, out)
	}
	return fmt.Errorf("unknown command %q", command)
}

func migrate(ctx context.Context, cfg config) error {
	if err := db.Bootstrap(ctx, cfg.ownerUrl, cfg.appPassword); err != nil {
		return err
	}
	if err := db.Migrate(cfg.ownerUrl, schema.Migrations); err != nil {
		return err
	}
	return jobs.Migrate(ctx, cfg.ownerUrl)
}

// usesDefaultAppPassword says whether the app role would connect with the
// password everyone can read in this repository.
func usesDefaultAppPassword(cfg config) bool {
	if cfg.appUrl == "" {
		return cfg.appPassword == "app"
	}
	// Any form Postgres accepts: a URL, a URL with ?password=, or
	// key=value pairs.
	parsed, err := pgx.ParseConfig(cfg.appUrl)
	if err != nil {
		// Unreadable is refused too: it cannot be shown to be safe.
		return true
	}
	return parsed.Password == "app"
}

func openApp(ctx context.Context, cfg config) (*pgxpool.Pool, error) {
	if cfg.appUrl != "" {
		return db.Open(ctx, cfg.appUrl)
	}
	appUrl, err := db.AppUrl(cfg.ownerUrl, cfg.appPassword)
	if err != nil {
		return nil, err
	}
	return db.Open(ctx, appUrl)
}

// newKernel wires the kernel with a queue. serve and demo start the queue
// and run its jobs; the command line only inserts them, so an approval
// made there is delivered once a server runs.
func newKernel(pool *pgxpool.Pool, queue *jobs.Queue, allowPrivate bool, publicUrl string, signer *sign.Signer) *pgmandate.Kernel {
	return pgmandate.New(pool, pgmandate.Options{Queue: queue, AllowPrivateConnectors: allowPrivate, PublicUrl: publicUrl, Signer: signer})
}

// signingKeys reads the key deliveries are signed with, and the public keys
// still published after a rotation. Without a key, on a loopback address,
// one is made in .local/signing-key; beyond loopback that is refused, so a
// server never signs with a key nobody chose.
func signingKeys(cfg config) (*sign.Signer, []ed25519.PublicKey, error) {
	text := os.Getenv("MANDATE_SIGNING_KEY")
	if file := os.Getenv("MANDATE_SIGNING_KEY_FILE"); text == "" && file != "" {
		raw, err := os.ReadFile(file) //nolint:gosec // the operator names the file
		if err != nil {
			return nil, nil, fmt.Errorf("MANDATE_SIGNING_KEY_FILE: %w", err)
		}
		text = string(raw)
	}
	if text == "" {
		if !loopback(cfg.httpAddr) {
			return nil, nil, errors.New("no signing key: set MANDATE_SIGNING_KEY or MANDATE_SIGNING_KEY_FILE to a 32-byte Ed25519 seed in standard base64")
		}
		var err error
		if text, err = localSigningKey(); err != nil {
			return nil, nil, err
		}
	}
	seed, err := sign.ParseSeed(text)
	if err != nil {
		return nil, nil, fmt.Errorf("signing key: %w", err)
	}
	signer, err := sign.NewSigner(seed)
	if err != nil {
		return nil, nil, err
	}
	var previous []ed25519.PublicKey
	for _, key := range splitList(os.Getenv("MANDATE_SIGNING_KEYS_PREVIOUS")) {
		public, err := sign.ParsePublicKey(key)
		if err != nil {
			return nil, nil, fmt.Errorf("MANDATE_SIGNING_KEYS_PREVIOUS: %w", err)
		}
		previous = append(previous, public)
	}
	return signer, previous, nil
}

// localSigningKey reads, or makes once, the key a local server signs with.
func localSigningKey() (string, error) {
	const path = ".local/signing-key"
	if raw, err := os.ReadFile(path); err == nil {
		return string(raw), nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	if err := os.MkdirAll(".local", 0o700); err != nil {
		return "", err
	}
	text := base64.StdEncoding.EncodeToString(seed)
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		return "", err
	}
	slog.Warn("made a signing key for local use in " + path + "; set MANDATE_SIGNING_KEY to sign with your own")
	return text, nil
}

// keysHandler publishes the public keys a receiver trusts, current first.
func keysHandler(signer *sign.Signer, previous []ed25519.PublicKey) http.Handler {
	body, _ := json.Marshal(sign.Publish(append([]ed25519.PublicKey{signer.PublicKey()}, previous...)...))
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_, _ = w.Write(body)
	})
}

func serve(ctx context.Context, cfg config) error {
	// With an app connection and no owner connection, the server never
	// holds the owner's credentials: an operator runs migrate separately.
	// Refused before anything is migrated, so a refusal changes nothing.
	if cfg.devLogin && !loopback(cfg.httpAddr) {
		return errors.New("MANDATE_DEV_LOGIN is allowed only when MANDATE_HTTP_ADDR is a loopback address")
	}
	if !loopback(cfg.httpAddr) && usesDefaultAppPassword(cfg) {
		return errors.New("serving beyond this machine with the default app password is refused; set MANDATE_APP_PASSWORD, or a password in MANDATE_APP_DATABASE_URL")
	}
	if cfg.appUrl == "" || cfg.ownerGiven {
		if err := migrate(ctx, cfg); err != nil {
			return err
		}
	}
	signer, previous, err := signingKeys(cfg)
	if err != nil {
		return err
	}
	pool, err := openApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	queue := jobs.NewQueue(pool)
	kernel := newKernel(pool, queue, cfg.allowPrivate, cfg.publicUrl, signer)
	if err := queue.Init(4); err != nil {
		return err
	}
	authService := auth.NewService(pool, auth.Config{DevLogin: cfg.devLogin, SessionTtl: cfg.sessionTtl, PublicUrl: cfg.publicUrl, Oidc: cfg.oidc})
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The queue gets a context the signal does not cancel: running jobs
	// finish their webhook call and record it, and Stop below waits for them.
	if err := queue.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	go sweepSessions(ctx, authService)

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.MaxBytesHandler(kernel.Handler(), maxMcpBytes))
	// The same agent contract as JSON over HTTP, for an agent that does not
	// speak MCP.
	agentPath, agentHandler := kernel.AgentHandler()
	mux.Handle(agentPath, http.MaxBytesHandler(agentHandler, maxMcpBytes))
	mux.Handle("GET /.well-known/pgmandate-keys", keysHandler(signer, previous))
	api.New(kernel, authService).Mount(mux)
	mux.Handle("/", web.Handler())
	// Healthy means the database answers: without it nothing is decided,
	// so a load balancer should send no one here.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	server := &http.Server{
		Addr: cfg.httpAddr, Handler: harden(mux, cfg),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	// Requests get a context that is cancelled a moment after shutdown
	// starts: ordinary calls finish, and an agent's long wait ends at once
	// rather than holding the shutdown or dying under a closed pool.
	requests, endRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer endRequests()
	server.BaseContext = func(net.Listener) context.Context { return requests }
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		time.AfterFunc(3*time.Second, endRequests)
		httpCtx, cancelHttp := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancelHttp()
		_ = server.Shutdown(httpCtx)
		queueCtx, cancelQueue := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancelQueue()
		if err := queue.Stop(queueCtx); err != nil {
			slog.Error("the job queue did not stop cleanly", "error", err)
		}
	}()
	slog.Info("pgmandate serving", "version", version.String(), "ui", cfg.publicUrl, "mcp", cfg.publicUrl+"/mcp", "devLogin", cfg.devLogin, "oidc", cfg.oidc != nil,
		"privateConnectors", cfg.allowPrivate)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns as soon as shutdown begins; wait for it, and
	// for the jobs, before the pool closes.
	<-done
	return nil
}

// maxMcpBytes bounds a request from an agent.
const maxMcpBytes = 1 << 20

// harden adds the headers every response carries and, when dev login is
// on, refuses requests that did not ask for this machine by name: a page
// elsewhere cannot then reach the dev login by rebinding its own name to
// 127.0.0.1.
func harden(next http.Handler, cfg config) http.Handler {
	secure := strings.HasPrefix(cfg.publicUrl, "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.devLogin && !loopbackHost(r.Host) {
			http.Error(w, "dev login is on: this server answers only to localhost", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; frame-ancestors 'none'")
		if secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost says whether a Host header names this machine.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// sweepSessions removes expired sessions once an hour.
func sweepSessions(ctx context.Context, authService *auth.Service) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if _, err := authService.DeleteExpired(ctx); err != nil && ctx.Err() == nil {
			slog.Error("removing expired sessions failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// receiver runs the reference receiver locally and prints each delivery it
// accepts: somewhere for a webhook action to land while you try things out.
func receiver(ctx context.Context, cfg config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("receiver", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	addr := flags.String("addr", cfg.receiverAddr, "address to listen on")
	tenant := flags.String("tenant", "", "the tenant whose deliveries it accepts")
	audiences := flags.String("audience", "", "comma separated audiences of the actions that point here")
	keyList := flags.String("keys", "", "comma separated public keys to trust, in standard base64")
	keysUrl := flags.String("keys-url", strings.TrimSuffix(cfg.publicUrl, "/")+"/.well-known/pgmandate-keys", "where to fetch the published keys")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("receiver: %w", err)
	}
	tenantId, err := uuid.Parse(*tenant)
	if err != nil || len(splitList(*audiences)) == 0 {
		return errors.New("receiver needs -tenant and -audience: the tenant id and the audiences of the actions that point here")
	}
	keys, err := receiverKeys(ctx, *keyList, *keysUrl)
	if err != nil {
		return err
	}
	r := webhook.NewReceiver(keys)
	if *keyList == "" {
		// Keys fetched from the server are fetched again on a key id the
		// receiver does not know, as after a rotation.
		r.Refresh = func(ctx context.Context) (sign.KeySet, error) { return sign.Fetch(ctx, nil, *keysUrl) }
	}
	for _, audience := range splitList(*audiences) {
		r.Trust(audience, tenantId)
	}
	r.OnReceive = func(received webhook.Received) {
		fmt.Fprint(out, describeDelivery(received))
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *addr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.WithoutCancel(ctx))
	}()
	fmt.Fprintf(out, "receiver listening on http://%s/ (signature, audience and tenant checked; each delivery done once)\n", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// receiverKeys are the keys named, or else the set the server publishes.
func receiverKeys(ctx context.Context, list, url string) (sign.KeySet, error) {
	if list != "" {
		var keys []ed25519.PublicKey
		for _, text := range splitList(list) {
			key, err := sign.ParsePublicKey(text)
			if err != nil {
				return nil, fmt.Errorf("-keys: %w", err)
			}
			keys = append(keys, key)
		}
		return sign.NewKeySet(keys...), nil
	}
	keys, err := sign.Fetch(ctx, nil, url)
	if err != nil {
		return nil, fmt.Errorf("%w (is the server running? or give -keys)", err)
	}
	return keys, nil
}

// modeText says what an agent's mode means.
func modeText(mode string) string {
	if mode == agents.ModeLive {
		return "live: what it proposes is carried out once allowed"
	}
	return "in dry run: what it proposes is recorded and never carried out, while it acts with its own credential"
}

// describeDelivery is what the receiver prints for a delivery: a line to
// scan, and the body exactly as it arrived.
func describeDelivery(received webhook.Received) string {
	p := received.Payload
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-9s %-18s %s\n", received.At.Format("15:04:05"), p.Decision.Status, p.Proposal.Action, p.Proposal.Title)
	fmt.Fprintf(&b, "          delivery %s  correlation %s\n", received.DeliveryId, p.CorrelationId)
	var body bytes.Buffer
	if err := json.Indent(&body, received.Body, "          ", "  "); err != nil {
		body.Reset()
		body.Write(received.Body)
	}
	fmt.Fprintf(&b, "          %s\n\n", body.String())
	return b.String()
}

// proposeCommand proposes as an agent, over MCP, the way any MCP client
// does: for trying an action and a receiver without an LLM.
func proposeCommand(ctx context.Context, cfg config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("propose", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	token := flags.String("token", os.Getenv("MANDATE_AGENT_TOKEN"), "the agent's token (mnd_...), or MANDATE_AGENT_TOKEN")
	endpoint := flags.String("url", strings.TrimSuffix(cfg.publicUrl, "/")+"/mcp", "the MCP address")
	action := flags.String("action", "", "the action key")
	title := flags.String("title", "", "a title for the person who may decide it")
	payload := flags.String("payload", "{}", "the payload, a JSON object")
	rationale := flags.String("rationale", "", "why: the account the person deciding reads")
	evidenceList := flags.String("evidence", "[]", `what was looked at, a JSON array of {"source", "ref", "summary"}`)
	runId := flags.String("run", "", "the run id; a new one when empty")
	key := flags.String("key", "", "the idempotency key; <run>:1 when empty. Send the same key to retry")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("propose: %w", err)
	}
	if *token == "" || *action == "" || *title == "" || *rationale == "" {
		return errors.New("propose needs -token, -action, -title and -rationale")
	}
	var evidence []map[string]string
	if err := json.Unmarshal([]byte(*evidenceList), &evidence); err != nil {
		return errors.New(`-evidence must be a JSON array of {"source", "ref", "summary"}`)
	}
	if *runId == "" {
		*runId = "cli-" + uuid.NewString()[:8]
	}
	if *key == "" {
		*key = *runId + ":1"
	}
	// The client's name and version reach the log as the proposal's
	// provenance: this command line, at this release.
	client := sdk.NewClient(&sdk.Implementation{Name: "pgmandate-cli", Version: version.String()}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: *endpoint, HTTPClient: &http.Client{Transport: bearer{*token}}, MaxRetries: -1,
	}, nil)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", *endpoint, err)
	}
	defer session.Close() //nolint:errcheck
	// The payload goes as written, so its numbers keep every digit; the
	// other arguments are encoded as JSON strings.
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*payload), &object); err != nil || object == nil {
		return errors.New("-payload must be a JSON object")
	}
	fields, err := json.Marshal(map[string]any{
		"action": *action, "title": *title, "run_id": *runId, "idempotency_key": *key, "rationale": *rationale, "evidence": evidence,
	})
	if err != nil {
		return err
	}
	arguments := json.RawMessage(strings.TrimSuffix(string(fields), "}") + `,"payload":` + *payload + `}`)
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "propose", Arguments: arguments})
	if err != nil {
		return err
	}
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			var pretty bytes.Buffer
			if json.Indent(&pretty, []byte(text.Text), "", "  ") == nil {
				fmt.Fprintln(out, pretty.String())
			} else {
				fmt.Fprintln(out, text.Text)
			}
		}
	}
	fmt.Fprintf(out, "run %s, key %s: send the same -run and -key to retry\n", *runId, *key)
	if result.IsError {
		return errors.New("refused")
	}
	return nil
}

// admin runs a person's command against the database, scoped to the tenant
// and the person named with -as (or as the system where no person acts).
func admin(ctx context.Context, cfg config, command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant", "", "tenant id")
	as := flags.String("as", "", "email of the person acting")
	name := flags.String("name", "", "name")
	owner := flags.String("owner", "", "owner email")
	ownerName := flags.String("owner-name", "", "owner name")
	email := flags.String("email", "", "email")
	role := flags.String("role", tenants.RoleReviewer, "owner or reviewer")
	agentId := flags.String("agent", "", "agent id; for grant, empty means every agent of the tenant")
	personId := flags.String("person", "", "person id")
	days := flags.Int("days", 90, "days")
	key := flags.String("key", "", "action key")
	description := flags.String("description", "", "action description")
	risk := flags.String("risk", mandates.RiskLow, "action risk")
	wait := flags.Duration("wait", actions.DefaultWaitLimit, "how long a proposal waits for a decision before it expires")
	deliverWithin := flags.Duration("deliver-within", actions.DefaultDeliverWithin, "how long after a decision its delivery may be carried out")
	mayExecute := flags.Bool("may-execute", false, "a mandate may let it run without a person")
	connector := flags.String("connector", actions.ConnectorWebhook, "webhook or builtin")
	url := flags.String("url", "", "webhook url")
	schemaText := flags.String("schema", "", "payload schema, JSON")
	schemaFile := flags.String("schema-file", "", "payload schema file")
	department := flags.String("department", "", "department label")
	condition := flags.String("condition", "", "a CEL condition on the payload that narrows the mandate")
	kind := flags.String("kind", "", "for notify: slack or discord")
	mode := flags.String("mode", "", "for agent create and agent mode: live or dry_run")
	perMinute := flags.Int("per-minute", 30, "for agent limits: proposals a minute")
	perHour := flags.Int("per-hour", 300, "for agent limits: proposals an hour")
	doneFlag := flags.String("done", "", "for settle: true if the receiver did it, false if not")
	actionList := flags.String("actions", "", "comma separated action patterns")
	maxRisk := flags.String("max-risk", mandates.RiskLow, "low or medium")
	budget := flags.Int64("budget", 0, "executions a mandate may allow per month")
	policy := flags.String("policy", "", "policy reference")
	mandateId := flags.String("mandate", "", "mandate id")
	proposalId := flags.String("proposal", "", "proposal id")
	note := flags.String("note", "", "decision note")
	confirm := flags.Bool("confirm", false, "confirm a high risk approval")
	status := flags.String("status", "", "proposal status")
	subject := flags.String("subject", "", "subject id")
	limit := flags.Int("limit", 50, "limit")
	all := flags.Bool("all", false, "include ended")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%s: %w\n%s", command, err, usage)
	}

	pool, err := openApp(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	queue := jobs.NewQueue(pool)
	kernel := newKernel(pool, queue, cfg.allowPrivate, cfg.publicUrl, nil)
	if err := queue.Init(1); err != nil {
		return err
	}

	if command == "tenant create" {
		if *name == "" || *owner == "" || *ownerName == "" {
			return errors.New("tenant create needs -name, -owner and -owner-name")
		}
		t, p, err := kernel.Tenants.CreateWithOwner(ctx, *name, *owner, *ownerName)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "tenant %s (%s)\nowner  %s (%s)\n", t.Id, t.Name, p.Id, p.Email)
		return nil
	}

	tenantId, err := uuid.Parse(*tenant)
	if err != nil {
		return errors.New("-tenant must be a tenant id")
	}
	scoped := actor.AsSystem(ctx, tenantId)
	if *as != "" {
		person, err := kernel.Tenants.FindPerson(scoped, *as)
		if err != nil {
			return err
		}
		scoped = actor.AsPerson(ctx, tenantId, person.Id)
	}

	switch command {
	case "person add":
		p, err := kernel.Tenants.AddPerson(scoped, *email, *name, *role)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "person %s (%s, %s)\n", p.Id, p.Email, p.Role)
	case "person remove":
		id, err := uuid.Parse(*personId)
		if err != nil {
			return errors.New("-person must be a person id")
		}
		if err := kernel.Tenants.RemovePerson(scoped, id); err != nil {
			return err
		}
		fmt.Fprintln(out, "removed")
	case "people":
		list, err := kernel.Tenants.ListPersons(scoped)
		if err != nil {
			return err
		}
		for _, p := range list {
			state := ""
			if p.RemovedAt != nil {
				state = " (removed)"
			}
			fmt.Fprintf(out, "person %s  %-8s %s <%s>%s\n", p.Id, p.Role, p.Name, p.Email, state)
		}
	case "redeliver":
		id, err := uuid.Parse(*proposalId)
		if err != nil {
			return errors.New("-proposal must be a proposal id")
		}
		d, err := kernel.Deliveries.SendAgain(scoped, id)
		if err != nil {
			return err
		}
		printDelivery(out, d)
	case "settle":
		id, err := uuid.Parse(*proposalId)
		if err != nil {
			return errors.New("-proposal must be a proposal id")
		}
		if *doneFlag != "true" && *doneFlag != "false" {
			return errors.New("-done must be true or false: whether the receiver did it")
		}
		d, err := kernel.Deliveries.Settle(scoped, id, *doneFlag == "true")
		if err != nil {
			return err
		}
		printDelivery(out, d)
	case "agent create":
		a, err := kernel.Agents.Create(scoped, *name, *department, *mode)
		if err != nil {
			return err
		}
		_, secret, err := kernel.Agents.IssueToken(scoped, a.Id, *days)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "agent %s (%s), %s\ntoken %s\n\nThe token is shown once. Give it to the agent as a bearer token for %s/mcp.\n", a.Id, a.Name, modeText(a.Mode), secret, strings.TrimSuffix(cfg.publicUrl, "/"))
	case "agent mode":
		id, err := uuid.Parse(*agentId)
		if err != nil {
			return errors.New("-agent must be an agent id")
		}
		a, err := kernel.Agents.SetMode(scoped, id, *mode)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "agent %s, %s\n", a.Id, modeText(a.Mode))
	case "agent revoke":
		id, err := uuid.Parse(*agentId)
		if err != nil {
			return errors.New("-agent must be an agent id")
		}
		if err := kernel.Agents.Revoke(scoped, id); err != nil {
			return err
		}
		fmt.Fprintln(out, "revoked")
	case "action define":
		input := actions.Input{Key: *key, Description: *description, Risk: *risk, WaitLimit: *wait, DeliverWithin: *deliverWithin, MayExecute: *mayExecute,
			Connector: *connector, Url: *url, Department: *department}
		if *schemaFile != "" {
			raw, err := os.ReadFile(*schemaFile) //nolint:gosec // a file the operator names on the command line
			if err != nil {
				return err
			}
			input.Schema = raw
		} else if *schemaText != "" {
			input.Schema = []byte(*schemaText)
		}
		defined, err := kernel.Actions.Define(scoped, input)
		if err != nil {
			return err
		}
		printAction(out, defined)
		if defined.ConnectorAudience != "" {
			fmt.Fprintf(out, "audience %s\n\nConfigure the receiver with this audience and the tenant id: it accepts nothing else. It verifies every delivery with the keys at %s/.well-known/pgmandate-keys.\n",
				defined.ConnectorAudience, strings.TrimSuffix(cfg.publicUrl, "/"))
		}
	case "agent limits":
		id, err := uuid.Parse(*agentId)
		if err != nil {
			return errors.New("-agent must be an agent id")
		}
		a, err := kernel.Agents.SetLimits(scoped, id, *perMinute, *perHour)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "agent %s may make %d proposals a minute and %d an hour\n", a.Id, a.MaxPerMinute, a.MaxPerHour)
	case "notify set":
		// The address is a secret: read from standard input when -url is
		// not given, so it stays out of the shell's history and ps.
		address := *url
		if address == "" {
			line, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && line == "" {
				return errors.New("give the incoming webhook's address on standard input, or with -url")
			}
			address = strings.TrimSpace(line)
		}
		n, err := kernel.Notifications.Set(scoped, *kind, address)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "notifications go to a %s channel (notifier %s)\n", n.Kind, n.Id)
	case "notify test":
		if err := kernel.Notifications.SendTest(scoped); err != nil {
			return err
		}
		fmt.Fprintln(out, "a test message was sent")
	case "notify remove":
		if err := kernel.Notifications.Remove(scoped); err != nil {
			return err
		}
		fmt.Fprintln(out, "notifications are off")
	case "action remove":
		if err := kernel.Actions.Remove(scoped, *key); err != nil {
			return err
		}
		fmt.Fprintln(out, "removed")
	case "actions":
		list, err := kernel.Actions.List(scoped, *all)
		if err != nil {
			return err
		}
		for _, e := range list {
			printAction(out, e)
		}
	case "grant":
		input := mandates.Input{
			Actions: splitList(*actionList), MaxRisk: *maxRisk, BudgetCount: *budget, PolicyRef: *policy, Days: *days, Department: *department, Condition: *condition,
		}
		if *agentId != "" {
			id, err := uuid.Parse(*agentId)
			if err != nil {
				return errors.New("-agent must be an agent id")
			}
			input.AgentId = &id
		}
		m, err := kernel.Mandates.Grant(scoped, input)
		if err != nil {
			return err
		}
		printMandate(out, m, 0)
	case "revoke":
		id, err := uuid.Parse(*mandateId)
		if err != nil {
			return errors.New("-mandate must be a mandate id")
		}
		if err := kernel.Mandates.Revoke(scoped, id); err != nil {
			return err
		}
		fmt.Fprintln(out, "revoked")
	case "mandates":
		list, err := kernel.Mandates.List(scoped, *all)
		if err != nil {
			return err
		}
		for _, m := range list {
			spent, err := kernel.Mandates.Spent(scoped, m.Id)
			if err != nil {
				return err
			}
			printMandate(out, m, spent)
		}
	case "proposals":
		list, err := kernel.Proposals.List(scoped, proposals.Filter{Status: *status, Limit: *limit})
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(list))
		for _, p := range list {
			ids = append(ids, p.Id)
		}
		delivered, err := kernel.Deliveries.ByProposals(scoped, ids)
		if err != nil {
			return err
		}
		for _, p := range list {
			printProposal(out, p)
			if d, ok := delivered[p.Id]; ok {
				printDelivery(out, d)
			}
		}
	case "approve", "reject", "dismiss":
		id, err := uuid.Parse(*proposalId)
		if err != nil {
			return errors.New("-proposal must be a proposal id")
		}
		var p proposals.Proposal
		switch command {
		case "approve":
			p, err = kernel.Proposals.Approve(scoped, id, proposals.Decision{Note: *note, Confirm: *confirm})
		case "reject":
			p, err = kernel.Proposals.Reject(scoped, id, *note)
		default:
			p, err = kernel.Proposals.Dismiss(scoped, id, *note)
		}
		if err != nil {
			return err
		}
		printProposal(out, p)
		if d, ok, err := kernel.Deliveries.Get(scoped, p.Id); err != nil {
			return err
		} else if ok {
			printDelivery(out, d)
		}
	case "events":
		var subjectId *uuid.UUID
		if *subject != "" {
			id, err := uuid.Parse(*subject)
			if err != nil {
				return errors.New("-subject must be an id")
			}
			subjectId = &id
		}
		return db.InTx(scoped, pool, func(tx pgx.Tx) error {
			list, err := events.List(scoped, tx, subjectId, *limit)
			if err != nil {
				return err
			}
			for _, e := range list {
				printEvent(out, e)
			}
			return nil
		})
	case "verify":
		var count int64
		err := db.InTx(scoped, pool, func(tx pgx.Tx) error {
			var err error
			count, err = events.Verify(scoped, tx)
			return err
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "chain intact: %d events\n", count)
	case "export":
		list, err := kernel.Proposals.Decided(scoped)
		if err != nil {
			return err
		}
		return proposals.WriteDecisions(out, list)
	case "export-chain":
		return db.InTx(scoped, pool, func(tx pgx.Tx) error {
			chain, err := events.Export(scoped, tx)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(out)
			for _, e := range chain {
				if err := encoder.Encode(e); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	return nil
}

// verifyExport recomputes an exported chain from a file, without the
// database: what an auditor does with what they were given.
func verifyExport(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("verify-export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant", "", "tenant id")
	file := flags.String("file", "", "the exported chain, JSON lines")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("verify-export: %w\n%s", err, usage)
	}
	tenantId, err := uuid.Parse(*tenant)
	if err != nil {
		return errors.New("-tenant must be a tenant id")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var chain []events.Exported
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for decoder.More() {
		var e events.Exported
		if err := decoder.Decode(&e); err != nil {
			return fmt.Errorf("line %d: %w", len(chain)+1, err)
		}
		chain = append(chain, e)
	}
	head, err := events.VerifyExported(tenantId, chain)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "chain intact: %d events, head %s\n", len(chain), head)
	return nil
}

func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func printMandate(out io.Writer, m mandates.Mandate, spent int64) {
	agent := "any agent"
	if m.AgentId != nil {
		agent = "agent " + m.AgentId.String()
	}
	state := "holds until " + m.ValidUntil.UTC().Format("2006-01-02")
	if m.RevokedAt != nil {
		state = "revoked " + m.RevokedAt.UTC().Format("2006-01-02")
	}
	fmt.Fprintf(out, "mandate %s: %s may %s up to %s risk, %d of %d executions this month, policy %q, %s\n",
		m.Id, agent, strings.Join(m.Actions, ", "), m.MaxRisk, spent, m.BudgetCount, m.PolicyRef, state)
	if m.Condition != "" {
		fmt.Fprintf(out, "  when %s\n", m.Condition)
	}
}

func printAction(out io.Writer, e actions.Action) {
	connector := e.ConnectorKind
	if e.ConnectorUrl != "" {
		connector += " " + e.ConnectorUrl
	}
	state := ""
	if e.RemovedAt != nil {
		state = " (removed)"
	}
	execute := "a person decides"
	if e.MayExecute {
		execute = "may execute under a mandate"
	}
	fmt.Fprintf(out, "action %-24s %-7s waits %s  %s  %s%s\n", e.Key, e.Risk, e.WaitLimit, execute, connector, state)
}

func printDelivery(out io.Writer, d deliveries.Delivery) {
	detail := fmt.Sprintf("after %d attempt(s), not after %s", d.AttemptsStarted, d.NotAfter.UTC().Format(time.RFC3339))
	if d.LastError != "" && d.Status != deliveries.StatusDone {
		detail += ": " + d.LastError
	}
	fmt.Fprintf(out, "%36s delivery %-9s %s %s\n", "", d.Status, d.Url, detail)
}

func printProposal(out io.Writer, p proposals.Proposal) {
	extra := ""
	switch {
	case p.ExecutionBasis != nil:
		extra = fmt.Sprintf(" under mandate %s (%s)", p.ExecutionBasis.MandateId, p.ExecutionBasis.PolicyRef)
	case len(p.Flags) > 0:
		extra = " flags " + strings.Join(p.Flags, ",")
	}
	if p.DryRun {
		extra += " (dry run: never carried out)"
	}
	fmt.Fprintf(out, "%s %-9s %-7s %-16s %s%s\n", p.Id, p.Status, p.Risk, p.Action, p.Title, extra)
}

func printEvent(out io.Writer, e events.Stored) {
	subject := ""
	if e.SubjectId != nil {
		subject = e.SubjectId.String()[:8]
	}
	fmt.Fprintf(out, "%4d %s %-7s %-28s %-8s %s\n", e.Seq, e.CreatedAt.UTC().Format("15:04:05"), e.ActorKind, e.Type, subject, string(e.Payload))
}
