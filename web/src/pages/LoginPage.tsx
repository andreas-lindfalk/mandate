import { ArrowRight } from "lucide-react";
import * as React from "react";
import { Navigate, useNavigate } from "react-router";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/primitives";
import { Wordmark } from "@/components/layout/AppShell";
import { useSession } from "@/lib/session";

export function LoginPage() {
  const session = useSession();
  const navigate = useNavigate();
  const [email, setEmail] = React.useState("");
  const [name, setName] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  if (session.signedIn && !session.loading) return <Navigate to="/" replace />;

  async function login() {
    setBusy(true);
    setError(null);
    try {
      await session.login(email.trim(), name.trim());
      navigate("/");
    } catch {
      setError("The sign-in failed. Is the server running locally with dev login on?");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-dvh lg:grid-cols-[1.1fr_1fr]">
      <section className="relative hidden flex-col justify-between overflow-hidden bg-espresso p-12 text-cream lg:flex">
        <div className="label-mono flex items-center gap-2 text-sand">
          <span className="size-2 bg-sand" aria-hidden /> pgmandate
        </div>
        <div className="relative z-10 max-w-lg">
          <h1 className="font-serif text-[52px] font-medium leading-[1.05] tracking-[-0.015em]">
            <span className="block">Agents propose.</span>
            <span className="block">You and your mandates decide.</span>
          </h1>
          <p className="mt-5 text-[17px] leading-relaxed text-cream/65">
            Write authority for AI agents. Agents hold no credentials and only propose. Mandates are rows, every
            decision names its authority, and <em className="font-serif text-cream">nothing runs without a person or a mandate a person granted</em>.
          </p>
        </div>
        <div className="label-mono flex gap-6 text-[11px] text-cream/65">
          <span>Hash chained log</span>
          <span>Signed deliveries</span>
          <span>Postgres enforced</span>
        </div>
      </section>
      <section className="flex items-center justify-center px-6 py-12">
        <div className="w-full max-w-md">
          <div className="mb-8 inline-flex rounded-[var(--radius-card)] bg-espresso px-3 py-2 lg:hidden">
            <Wordmark />
          </div>
          <p className="label-mono text-accent">Sign in</p>
          <h2 className="mt-2 font-serif text-3xl font-medium">Sign in to pgmandate</h2>
          {session.oidc ? (
            <div className="mt-7">
              <Button asChild size="lg" className="w-full">
                <a href="/auth/oidc/start">
                  Sign in with your organisation <ArrowRight />
                </a>
              </Button>
            </div>
          ) : null}
          {session.devLogin ? (
            <form
              className="mt-7 flex flex-col gap-4 border-t border-border pt-6"
              onSubmit={(event) => {
                event.preventDefault();
                if (email.trim()) login();
              }}
            >
              <p className="text-sm text-muted">Dev login: locally an email address is enough.</p>
              <Field label="Email" htmlFor="email">
                <Input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="alice@acme.example" autoFocus />
              </Field>
              <Field label="Name" htmlFor="name">
                <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Alice" />
              </Field>
              <Button type="submit" variant="secondary" loading={busy} disabled={!email.trim()}>
                Sign in
              </Button>
            </form>
          ) : null}
          {!session.devLogin && !session.oidc && !session.loading ? (
            <p className="mt-7 text-sm text-muted">No sign-in method is configured. Set MANDATE_OIDC_ISSUER, or MANDATE_DEV_LOGIN=true on localhost.</p>
          ) : null}
          {error ? (
            <p role="alert" className="mt-4 text-sm text-danger">
              {error}
            </p>
          ) : null}
        </div>
      </section>
    </div>
  );
}

export function TenantPickerPage() {
  const session = useSession();
  const navigate = useNavigate();
  return (
    <div className="flex min-h-dvh items-center justify-center px-6">
      <div className="w-full max-w-md">
        <p className="label-mono text-accent">Choose a tenant</p>
        <h1 className="mt-2 font-serif text-3xl font-medium">Where do you want to work?</h1>
        <ul className="mt-6 flex flex-col gap-2">
          {session.tenants.map((tenant) => (
            <li key={tenant.tenantId}>
              <button
                type="button"
                onClick={() => {
                  session.selectTenant(tenant.tenantId);
                  navigate("/inbox");
                }}
                className="flex w-full items-center justify-between rounded-[var(--radius-card)] border border-border bg-surface px-4 py-3.5 text-left hover:border-accent/50"
              >
                <span className="font-medium">{tenant.tenantName}</span>
                <ArrowRight className="size-4 text-muted" />
              </button>
            </li>
          ))}
        </ul>
        {session.tenants.length === 0 ? (
          <div className="mt-6 text-sm text-muted">
            <p>
              You are signed in as {session.email}, but no tenant has you as a person. An owner adds you, or an operator creates a tenant with
              you as its owner: <code className="font-mono text-[12px]">pgmandate tenant create -name … -owner {session.email} -owner-name …</code>
            </p>
            <Button variant="link" className="mt-4" onClick={() => session.logout().then(() => navigate("/login"))}>
              Sign out
            </Button>
          </div>
        ) : null}
      </div>
    </div>
  );
}
