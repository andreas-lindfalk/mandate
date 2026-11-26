import { CircleAlert, RotateCw } from "lucide-react";
import * as React from "react";
import { Button } from "@/components/ui/button";
import { errorText } from "@/lib/errors";
import { cn } from "@/lib/utils";

export function Card({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("rounded-[var(--radius-card)] border border-border bg-surface", className)} {...props} />;
}

export function Badge({
  tone = "neutral",
  className,
  ...props
}: React.HTMLAttributes<HTMLSpanElement> & {
  tone?: "neutral" | "danger" | "warning" | "success" | "accent";
}) {
  const tones = {
    neutral: "bg-sunken text-foreground border-border",
    danger: "bg-danger-soft text-danger-ink border-danger/25",
    warning: "bg-warning-soft text-warning-ink border-warning/30",
    success: "bg-success-soft text-success border-success/25",
    accent: "bg-accent-wash text-accent border-sand",
  } as const;
  return (
    <span
      className={cn(
        "inline-flex h-6 items-center gap-1 rounded-[5px] border px-2 font-mono text-[11px] font-medium uppercase tracking-[0.06em] [&_svg]:size-3",
        tones[tone],
        className,
      )}
      {...props}
    />
  );
}

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...props }, ref) => (
    <input
      ref={ref}
      className={cn(
        "h-10 w-full rounded-[var(--radius-control)] border border-input-border bg-surface px-3 text-sm text-foreground placeholder:text-muted transition-colors focus:border-accent disabled:bg-sunken",
        className,
      )}
      {...props}
    />
  ),
);
Input.displayName = "Input";

export const Textarea = React.forwardRef<HTMLTextAreaElement, React.TextareaHTMLAttributes<HTMLTextAreaElement>>(
  ({ className, ...props }, ref) => (
    <textarea
      ref={ref}
      className={cn(
        "min-h-24 w-full rounded-[var(--radius-control)] border border-input-border bg-surface px-3 py-2 text-sm leading-relaxed text-foreground placeholder:text-muted transition-colors focus:border-accent disabled:bg-sunken",
        className,
      )}
      {...props}
    />
  ),
);
Textarea.displayName = "Textarea";

export function Field({
  label,
  hint,
  htmlFor,
  children,
  className,
}: {
  label: string;
  hint?: React.ReactNode;
  htmlFor?: string;
  children: React.ReactNode;
  className?: string;
}) {
  // The hint is tied to the field, so a screen reader reads it with it.
  const hintId = React.useId();
  // An id the control already points at is kept beside the hint's.
  const control =
    hint && React.isValidElement<{ "aria-describedby"?: string }>(children)
      ? React.cloneElement(children, { "aria-describedby": [children.props["aria-describedby"], hintId].filter(Boolean).join(" ") })
      : children;
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <label htmlFor={htmlFor} className="text-[13px] font-medium text-foreground">
        {label}
      </label>
      {control}
      {hint ? (
        <p id={hintId} className="text-xs text-muted">
          {hint}
        </p>
      ) : null}
    </div>
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div className={cn("animate-pulse rounded-md bg-sunken", className)} aria-hidden />;
}

export function SectionLabel({ children }: { children: React.ReactNode }) {
  return <div className="label-mono text-accent">{children}</div>;
}

export function EmptyState({ icon, title, children }: { icon: React.ReactNode; title: string; children?: React.ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 rounded-[var(--radius-card)] border border-dashed border-border-strong px-6 py-14 text-center">
      <div className="text-sand [&_svg]:size-8">{icon}</div>
      <p className="font-serif text-xl">{title}</p>
      {children ? <div className="max-w-md text-sm text-muted">{children}</div> : null}
    </div>
  );
}

/** A request that failed, said plainly, with a way to try again. Never an empty state. */
export function ErrorState({
  title = "Could not load this",
  error,
  onRetry,
  className,
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center gap-3 rounded-[var(--radius-card)] border border-danger/25 bg-danger-soft/40 px-6 py-10 text-center",
        className,
      )}
    >
      <CircleAlert className="size-7 text-danger" aria-hidden />
      <p className="font-serif text-xl">{title}</p>
      <p className="max-w-md text-sm text-muted">{errorText(error)}</p>
      {onRetry ? (
        <Button variant="secondary" size="sm" onClick={onRetry}>
          <RotateCw /> Try again
        </Button>
      ) : null}
    </div>
  );
}

/**
 * A refetch failed while data from an earlier load is still at hand: the
 * data stays on screen and this says it may be out of date.
 */
export function StaleNote({ error, className }: { error: unknown; className?: string }) {
  return (
    <p role="status" title={errorText(error)} className={cn("flex items-center gap-1.5 text-[12px] text-warning-ink", className)}>
      <CircleAlert className="size-3.5 shrink-0" aria-hidden /> Could not refresh; showing what was loaded.
    </p>
  );
}
