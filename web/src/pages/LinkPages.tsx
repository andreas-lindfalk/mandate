import { ArrowRight } from "lucide-react";
import * as React from "react";
import { Link, useNavigate } from "react-router";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/primitives";
import { api } from "@/lib/api";
import { errorText } from "@/lib/errors";
import { useSession } from "@/lib/session";

/** The token after "#": it never leaves the browser except in the call that uses it. */
function tokenFromFragment() {
  return window.location.hash.replace(/^#/, "");
}

function Frame({ eyebrow, title, children }: { eyebrow: string; title: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-dvh items-center justify-center px-6 py-12">
      <div className="w-full max-w-md">
        <p className="label-mono text-accent">{eyebrow}</p>
        <h1 className="mt-2 font-serif text-3xl font-medium">{title}</h1>
        {children}
      </div>
    </div>
  );
}

/** The setup link's page: the first organisation and its owner. */
export function SetupPage() {
  const session = useSession();
  const navigate = useNavigate();
  const [token] = React.useState(tokenFromFragment);
  const [organisation, setOrganisation] = React.useState("");
  const [name, setName] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  // The token is kept in memory; the address bar no longer shows it.
  React.useEffect(() => {
    if (window.location.hash) window.history.replaceState(null, "", window.location.pathname);
  }, []);

  async function setUp() {
    setBusy(true);
    setError(null);
    try {
      await api.auth.setUp({ token, organisation: organisation.trim(), name: name.trim(), email: email.trim() });
      session.signedInByCookie();
      navigate("/");
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }

  if (!token) {
    return (
      <Frame eyebrow="Set up" title="This link has no token">
        <p className="mt-4 text-sm text-muted">Open the whole setup link from the server's log, including the part after “#”.</p>
      </Frame>
    );
  }
  return (
    <Frame eyebrow="Set up pgmandate" title="Your organisation, and you as its owner">
      <form
        className="mt-6 flex flex-col gap-4"
        onSubmit={(event) => {
          event.preventDefault();
          setUp();
        }}
      >
        <Field label="Organisation" htmlFor="organisation">
          <Input id="organisation" value={organisation} onChange={(e) => setOrganisation(e.target.value)} placeholder="Acme" autoFocus maxLength={200} />
        </Field>
        <Field label="Your name" htmlFor="name">
          <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Alice" maxLength={200} />
        </Field>
        <Field label="Your email" htmlFor="email" hint="You sign in as this person. A sign-in provider you add later binds to it.">
          <Input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="alice@acme.example" maxLength={200} />
        </Field>
        <Button type="submit" loading={busy} disabled={!organisation.trim() || !name.trim() || !email.trim()}>
          Set up and sign in <ArrowRight />
        </Button>
        {error ? (
          <p role="alert" className="text-sm text-danger-ink">
            {error}
          </p>
        ) : null}
      </form>
    </Frame>
  );
}

/** A person's single-use link: signs in at once. */
export function SignInLinkPage() {
  const session = useSession();
  const navigate = useNavigate();
  const [error, setError] = React.useState<string | null>(null);
  const started = React.useRef(false);
  React.useEffect(() => {
    // Once, even in development's double render: the link works once.
    if (started.current) return;
    started.current = true;
    const token = tokenFromFragment();
    window.history.replaceState(null, "", window.location.pathname);
    api.auth
      .redeemLink({ token })
      .then(() => {
        session.signedInByCookie();
        navigate("/");
      })
      .catch((e: unknown) => setError(errorText(e)));
  }, [session, navigate]);
  return (
    <Frame eyebrow="Sign in" title={error ? "This link does not work" : "Signing you in…"}>
      {error ? (
        <>
          <p role="alert" className="mt-4 text-sm text-danger-ink">
            {error}
          </p>
          <Link to="/login" className="mt-4 inline-block text-sm underline underline-offset-2">
            To the sign-in page
          </Link>
        </>
      ) : null}
    </Frame>
  );
}
