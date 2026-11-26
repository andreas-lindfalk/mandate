import { type ClassValue, clsx } from "clsx";
import * as React from "react";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// A click handler for the second step of a two-step confirmation. The first
// click of a double-click arms the confirmation; the second one arrives with
// a detail of 2 and is dropped, so a double-click never carries it out. A key
// press has a detail of 0 and goes through.
export function singleClick(run: () => void) {
  return (event: { detail: number }) => {
    if (event.detail <= 1) run();
  };
}

// Puts focus back on what opened a two-step confirmation or an inline editor
// once it closes. Closing unmounts the Cancel button that had focus, which
// drops focus to the page; the trigger, rendered again in its place, takes it
// back. `open` is the open item's key (or a boolean when there is one), and
// the returned function gives each trigger its ref by that key. Focus that
// has already moved elsewhere, as to a newly armed confirmation, stays there.
export function useFocusBack(open: string | boolean | null) {
  const triggers = React.useRef(new Map<string, HTMLElement>());
  const previous = React.useRef(open);
  React.useEffect(() => {
    const closed = previous.current;
    previous.current = open;
    if (closed === null || closed === false || closed === open) return;
    const active = document.activeElement;
    if (active && active !== document.body) return;
    triggers.current.get(String(closed))?.focus();
  }, [open]);
  return (key: string | boolean = true) =>
    (element: HTMLElement | null) => {
      if (element) triggers.current.set(String(key), element);
      else triggers.current.delete(String(key));
    };
}
