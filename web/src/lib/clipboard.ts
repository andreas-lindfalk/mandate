/**
 * Copies text and says whether it worked. The async clipboard API is missing
 * outside a secure context (plain http on a LAN, an on-premise install), so
 * the old selection trick is the fallback.
 */
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      /* denied or not focused: try the fallback */
    }
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.top = "0";
  area.style.left = "0";
  area.style.opacity = "0";
  document.body.appendChild(area);
  const active = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  try {
    area.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    area.remove();
    active?.focus();
  }
}
