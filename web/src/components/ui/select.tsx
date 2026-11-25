import * as SelectPrimitive from "@radix-ui/react-select";
import { Check, ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

export function Select({
  value,
  onValueChange,
  options,
  placeholder,
  id,
  className,
  disabled,
  "aria-describedby": describedBy,
}: {
  value: string;
  onValueChange: (value: string) => void;
  options: { value: string; label: string }[];
  placeholder?: string;
  id?: string;
  className?: string;
  disabled?: boolean;
  "aria-describedby"?: string;
}) {
  return (
    <SelectPrimitive.Root value={value || undefined} onValueChange={onValueChange} disabled={disabled}>
      <SelectPrimitive.Trigger
        aria-describedby={describedBy}
        id={id}
        className={cn(
          "flex h-10 w-full items-center justify-between gap-2 rounded-[var(--radius-control)] border border-input-border bg-surface px-3 text-left text-sm focus:border-accent disabled:bg-sunken data-[placeholder]:text-muted",
          className,
        )}
      >
        <SelectPrimitive.Value placeholder={placeholder} />
        <SelectPrimitive.Icon>
          <ChevronDown className="size-4 text-muted" />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          position="popper"
          sideOffset={4}
          className="z-50 min-w-[var(--radix-select-trigger-width)] overflow-hidden rounded-[var(--radius-control)] border border-border bg-surface shadow-popover"
        >
          <SelectPrimitive.Viewport className="p-1">
            {options.map((option) => (
              <SelectPrimitive.Item
                key={option.value}
                value={option.value}
                className="relative flex h-9 cursor-pointer select-none items-center rounded-[4px] pl-8 pr-3 text-sm outline-none data-[highlighted]:bg-sunken"
              >
                <SelectPrimitive.ItemIndicator className="absolute left-2.5">
                  <Check className="size-3.5 text-accent" />
                </SelectPrimitive.ItemIndicator>
                <SelectPrimitive.ItemText>{option.label}</SelectPrimitive.ItemText>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}
