import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { Bot, ChevronsUpDown, FileCheck, Inbox, LogOut, Menu, ScrollText, ShieldAlert, ShieldCheck, ShieldQuestion, Users, X } from "lucide-react";
import * as React from "react";
import { NavLink, Outlet, useNavigate } from "react-router";
import { api } from "@/lib/api";
import { roleLabel } from "@/lib/format";
import { useSession } from "@/lib/session";
import { cn } from "@/lib/utils";

export function Wordmark({ className }: { className?: string }) {
  return (
    <div className={cn("flex items-center gap-2.5", className)}>
      <svg viewBox="0 0 32 32" className="size-7" aria-hidden>
        <rect width="32" height="32" rx="7" className="fill-espresso-2" />
        <path d="M9 22V10l7 7 7-7v12" fill="none" className="stroke-sand" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <span className="text-[15px] font-semibold tracking-tight text-cream">pgmandate</span>
    </div>
  );
}

function NavItem({ to, icon, label, count, onNavigate }: { to: string; icon: React.ReactNode; label: string; count?: number; onNavigate?: () => void }) {
  return (
    <NavLink
      to={to}
      onClick={onNavigate}
      className={({ isActive }) =>
        cn(
          "group flex h-9 items-center gap-3 rounded-md px-3 text-[14px] transition-colors duration-150",
          isActive ? "bg-cream/10 text-cream" : "text-cream/65 hover:bg-cream/5 hover:text-cream",
        )
      }
    >
      {({ isActive }) => (
        <>
          <span className={cn("[&_svg]:size-4", isActive ? "text-sand" : "")}>{icon}</span>
          <span className="flex-1">{label}</span>
          {count ? (
            <span className="min-w-6 rounded-full bg-sand px-1.5 text-center font-mono text-[11px] font-medium text-espresso">
              {count}
              <span className="sr-only"> waiting</span>
            </span>
          ) : null}
        </>
      )}
    </NavLink>
  );
}

function TenantSwitcher({ onSelect }: { onSelect?: () => void }) {
  const session = useSession();
  const [open, setOpen] = React.useState(false);
  const ref = React.useRef<HTMLDivElement>(null);
  React.useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);
  if (!session.tenant) return null;
  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-3 rounded-md border border-cream/10 bg-cream/5 px-3 py-2.5 text-left hover:bg-cream/10"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <div className="min-w-0 flex-1">
          <p className="truncate text-[14px] font-medium text-cream">{session.tenant.tenantName}</p>
          <p className="label-mono text-[11px] text-cream/65">{roleLabel[session.tenant.role] ?? session.tenant.role}</p>
        </div>
        <ChevronsUpDown className="size-4 text-cream/50" />
      </button>
      {open ? (
        <ul role="listbox" className="absolute left-0 right-0 top-full z-20 mt-1 overflow-hidden rounded-md border border-cream/10 bg-espresso-2 py-1 shadow-popover">
          {session.tenants.map((tenant) => (
            <li key={tenant.tenantId}>
              <button
                type="button"
                role="option"
                aria-selected={tenant.tenantId === session.tenant?.tenantId}
                onClick={() => {
                  session.selectTenant(tenant.tenantId);
                  setOpen(false);
                  onSelect?.();
                }}
                className="flex w-full items-center justify-between px-3 py-2 text-left text-[14px] text-cream/80 hover:bg-cream/5 hover:text-cream"
              >
                <span className="truncate">{tenant.tenantName}</span>
                <span className="label-mono text-[11px] text-cream/65">{roleLabel[tenant.role] ?? tenant.role}</span>
              </button>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function ChainStatus() {
  const chain = useQuery({ queryKey: ["chain"], queryFn: () => api.evidence.verifyChain({}), refetchInterval: 60_000 });
  if (chain.isError) {
    return (
      <div className="flex items-center gap-2 text-[12px] text-cream/55" title="Trying again in a minute.">
        <ShieldQuestion className="size-3.5" />
        <span>Could not check the chain right now</span>
      </div>
    );
  }
  if (!chain.data) return null;
  const ok = chain.data.ok;
  return (
    <div className={cn("flex items-center gap-2 text-[12px]", ok ? "text-cream/55" : "text-danger-on-dark")} title="Every event is hash linked to the one before it. The chain is recomputed from the start.">
      {ok ? <ShieldCheck className="size-3.5 text-sand" /> : <ShieldAlert className="size-3.5" />}
      <span>
        {ok ? "Chain verified" : "The chain is broken"} · {Number(chain.data.events)} events
      </span>
    </div>
  );
}

function SidebarContent({ onNavigate }: { onNavigate?: () => void }) {
  const session = useSession();
  const navigate = useNavigate();
  // Only the count: the total comes back whatever the limit.
  const pending = useQuery({
    queryKey: ["proposals", "pending", "count"],
    queryFn: () => api.proposals.listProposals({ status: "pending", limit: 1 }),
    refetchInterval: 10_000,
  });
  return (
    <>
      <div className="px-2">
        <Wordmark />
      </div>
      <div className="mt-6">
        <TenantSwitcher onSelect={onNavigate} />
      </div>
      <nav className="mt-6 flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto" aria-label="Main">
        <NavItem to="/inbox" icon={<Inbox />} label="Inbox" count={pending.data ? Number(pending.data.total) : undefined} onNavigate={onNavigate} />
        <NavItem to="/agents" icon={<Bot />} label="Agents & actions" onNavigate={onNavigate} />
        <NavItem to="/mandates" icon={<ScrollText />} label="Mandates" onNavigate={onNavigate} />
        <NavItem to="/people" icon={<Users />} label="People" onNavigate={onNavigate} />
        <NavItem to="/evidence" icon={<FileCheck />} label="Evidence" onNavigate={onNavigate} />
      </nav>
      <div className="flex flex-col gap-3 border-t border-cream/10 px-2 pt-4">
        <ChainStatus />
        <p className="truncate text-[12px] text-cream/55" title={session.email}>
          {session.name || session.email}
        </p>
        <button
          type="button"
          onClick={async () => {
            try {
              await session.logout();
            } catch {
              /* the session is gone from this browser either way */
            }
            onNavigate?.();
            navigate("/login");
          }}
          className="flex items-center gap-2 text-[13px] text-cream/55 hover:text-cream"
        >
          <LogOut className="size-3.5" /> Sign out
        </button>
      </div>
    </>
  );
}

export function AppShell() {
  const [menuOpen, setMenuOpen] = React.useState(false);
  const close = React.useCallback(() => setMenuOpen(false), []);
  React.useEffect(() => {
    if (!menuOpen) return;
    const wide = window.matchMedia("(min-width: 1024px)");
    const onChange = () => {
      if (wide.matches) setMenuOpen(false);
    };
    wide.addEventListener("change", onChange);
    return () => wide.removeEventListener("change", onChange);
  }, [menuOpen]);
  return (
    <div className="flex min-h-dvh flex-col lg:flex-row">
      <aside className="sticky top-0 hidden h-dvh w-[248px] shrink-0 flex-col bg-espresso px-3 py-5 lg:flex">
        <SidebarContent />
      </aside>
      <DialogPrimitive.Root open={menuOpen} onOpenChange={setMenuOpen}>
        <header className="sticky top-0 z-30 flex h-14 shrink-0 items-center justify-between bg-espresso pl-4 pr-2 lg:hidden">
          <Wordmark />
          <DialogPrimitive.Trigger className="flex size-10 items-center justify-center rounded-md text-cream/80 hover:bg-cream/10 hover:text-cream" aria-label="Open the menu">
            <Menu className="size-5" />
          </DialogPrimitive.Trigger>
        </header>
        <DialogPrimitive.Portal>
          <DialogPrimitive.Overlay className="fixed inset-0 z-40 bg-espresso/40 backdrop-blur-[2px] lg:hidden" />
          <DialogPrimitive.Content aria-describedby={undefined} className="fixed inset-y-0 left-0 z-50 flex w-[min(280px,calc(100vw-48px))] flex-col bg-espresso px-3 py-5 shadow-overlay lg:hidden">
            <DialogPrimitive.Title className="sr-only">Menu</DialogPrimitive.Title>
            <SidebarContent onNavigate={close} />
            <DialogPrimitive.Close className="absolute right-2 top-3.5 flex size-10 items-center justify-center rounded-md text-cream/70 hover:bg-cream/10 hover:text-cream" aria-label="Close the menu">
              <X className="size-4" />
            </DialogPrimitive.Close>
          </DialogPrimitive.Content>
        </DialogPrimitive.Portal>
      </DialogPrimitive.Root>
      <main className="min-w-0 flex-1">
        <Outlet />
      </main>
    </div>
  );
}

export function PageHeader({ eyebrow, title, children }: { eyebrow: React.ReactNode; title: React.ReactNode; children?: React.ReactNode }) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-4 border-b border-border px-6 pb-6 pt-9 lg:px-8">
      <div>
        <div className="label-mono mb-2 flex items-center gap-2 text-accent">{eyebrow}</div>
        <h1 className="font-serif text-[34px] font-medium leading-[1.1] tracking-[-0.01em]">{title}</h1>
      </div>
      {children ? <div className="flex items-center gap-2">{children}</div> : null}
    </header>
  );
}
