// Records the README's two short films of the web UI by driving the real UI
// in a browser, with a caption saying what happens:
//
// - docs/ui-inbox.gif: a refund waits for a person, who reads what
//   pgmandate checked and what the agent claims, and approves it; the
//   receiver carries it out and its answer is kept.
// - docs/ui-mandate.gif: a ticket close waits, since no mandate covers it;
//   the owner grants one; the agent's next close is carried out at once,
//   and the inbox shows which mandate allowed it.
//
// Nothing is staged: it signs in and clicks as a person would, and the
// agent proposes over the agent API with its own token. It needs what the
// demo leaves behind, a server with dev login and the demo's receiver:
//
//   make demo                     # then, in two more terminals:
//   make run
//   go run ./cmd/pgmandate receiver ...   # the command the demo printed
//   MANDATE_AGENT_TOKEN=mnd_... make demo-ui-gifs   # the token it printed
//
// It needs Playwright's Chromium (pnpm exec playwright install chromium)
// and ffmpeg. A run approves the demo's refund and grants a mandate, so each
// recording starts from a clean database with one demo tenant:
// `make reset && make up && make demo`.
import { chromium } from "playwright";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const base = process.env.MANDATE_PUBLIC_URL ?? "http://127.0.0.1:8585";
const agentToken = process.env.MANDATE_AGENT_TOKEN;
const docs = resolve(import.meta.dirname, "../../docs");
const viewport = { width: 1120, height: 700 };

// A pointer the viewer can follow, since headless video draws none, and a
// caption bar over the page header, both in the UI's own colours.
const overlay = `
  addEventListener("DOMContentLoaded", () => {
    const dot = document.createElement("div");
    dot.style.cssText = "position:fixed;z-index:2147483647;width:18px;height:18px;margin:-9px 0 0 -9px;border-radius:50%;" +
      "background:color-mix(in srgb, var(--color-accent) 35%, transparent);border:2px solid var(--color-accent);" +
      "pointer-events:none;left:-40px;top:-40px;transition:transform .12s";
    document.body.appendChild(dot);
    addEventListener("mousemove", (e) => { dot.style.left = e.clientX + "px"; dot.style.top = e.clientY + "px"; }, true);
    addEventListener("mousedown", () => { dot.style.transform = "scale(.7)"; }, true);
    addEventListener("mouseup", () => { dot.style.transform = ""; }, true);
    const caption = document.createElement("div");
    caption.id = "film-caption";
    caption.style.cssText = "position:fixed;z-index:2147483646;top:14px;left:268px;right:20px;padding:12px 18px;border-radius:10px;" +
      "background:var(--color-espresso);color:var(--color-cream);font:600 19px/1.35 -apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;" +
      "pointer-events:none;opacity:0;transition:opacity .25s";
    document.body.appendChild(caption);
  });`;

async function film(name, scenes) {
  const dir = mkdtempSync(join(tmpdir(), "pgmandate-ui-"));
  const browser = await chromium.launch();
  // Signed in before the film starts, so it opens on the page that matters.
  const signedIn = await browser.newContext({ viewport });
  const login = await signedIn.newPage();
  await login.goto(base + "/login");
  await login.fill("#email", "alice@acme.example");
  await login.getByRole("button", { name: /sign in|continue|dev/i }).last().click();
  const picker = login.getByText("Choose a tenant");
  await Promise.race([login.waitForURL("**/inbox"), picker.waitFor()]);
  if (await picker.isVisible()) {
    throw new Error("alice@acme.example is in more than one tenant: record from a clean database (make reset && make up && make demo)");
  }
  await login.waitForURL("**/inbox");
  const storageState = await signedIn.storageState();
  await signedIn.close();

  const context = await browser.newContext({ viewport, storageState, recordVideo: { dir, size: viewport } });
  await context.addInitScript(overlay);
  const page = await context.newPage();
  try {
    await scenes(page);
  } finally {
    await context.close();
    await browser.close();
  }
  const video = join(dir, readdirSync(dir).find((f) => f.endsWith(".webm")));
  const gif = join(docs, name);
  execFileSync("ffmpeg", ["-v", "error", "-y", "-i", video, "-vf",
    // The UI is flat colour: no dithering, few colours, and frames that
    // do not change dropped, keep each film near 4 MB.
    "mpdecimate,fps=8,scale=800:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=64[p];[b][p]paletteuse=dither=none:diff_mode=rectangle", gif]);
  if (process.env.KEEP_VIDEO) execFileSync("cp", [video, join(process.env.KEEP_VIDEO, name.replace(".gif", ".webm"))]);
  rmSync(dir, { recursive: true, force: true });
  console.log("wrote", gif);
}

const pause = (page, ms) => page.waitForTimeout(ms);

// Shows a caption and holds it long enough to be read, at about two and a
// half words a second, before anything on the page moves.
async function caption(page, text) {
  await page.evaluate((text) => {
    const c = document.getElementById("film-caption");
    c.textContent = text;
    c.style.opacity = text ? "1" : "0";
  }, text);
  await pause(page, 1500 + 400 * text.split(/\s+/).length);
}

// Moves the pointer to the element, so the viewer sees where, then clicks.
async function click(page, locator) {
  await locator.scrollIntoViewIfNeeded();
  const box = await locator.boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 35 });
  await pause(page, 600);
  await page.mouse.down();
  await page.mouse.up();
}

async function type(page, locator, text) {
  await click(page, locator);
  await locator.pressSequentially(text, { delay: 70 });
}

// Scrolls smoothly, as a reader would.
async function scroll(page, pixels) {
  await page.mouse.move(900, 450, { steps: 10 });
  for (let done = 0; done < Math.abs(pixels); done += 20) {
    await page.mouse.wheel(0, Math.sign(pixels) * 20);
    await pause(page, 25);
  }
}

// The agent proposes over the agent API, with its own token, as any agent
// or script that does not speak MCP would.
async function propose(request) {
  const response = await fetch(base + "/mandate.agent.v1.AgentService/Propose", {
    method: "POST",
    headers: { Authorization: "Bearer " + agentToken, "Content-Type": "application/json" },
    body: JSON.stringify(request),
  });
  if (!response.ok) throw new Error("propose: " + response.status + " " + (await response.text()));
  return response.json();
}

const only = process.env.ONLY;
if ((!only || only === "mandate") && !agentToken) {
  throw new Error("set MANDATE_AGENT_TOKEN to the token the demo printed: the mandate film has the agent propose");
}

if (!only || only === "inbox") await film("ui-inbox.gif", async (page) => {
  await page.goto(base + "/inbox");
  await caption(page, "The inbox: what the agents proposed and no mandate allows waits here for a person");
  await click(page, page.getByText("Refund the printer customer"));
  await caption(page, "A refund is high risk, so it always waits for a person, however many mandates there are");
  await scroll(page, 520);
  await caption(page, "First what pgmandate checked. Then, set apart, what the agent claims: shown to Alice, never trusted");
  await scroll(page, 360);
  await caption(page, "Alice approves, with a note that is kept with the decision");
  await type(page, page.locator("textarea").first(), "Confirmed with the customer: three weeks without a printer.");
  await click(page, page.getByText("I confirm this high risk action"));
  const approved = page.waitForResponse((r) => r.url().includes("ApproveProposal") && r.ok());
  await click(page, page.getByRole("button", { name: "Approve" }));
  await approved;
  await caption(page, "The approval goes, signed, to the receiver next to the helpdesk, which holds the credential");
  await scroll(page, -2000);
  await click(page, page.getByText(/^Approved$/).first());
  await pause(page, 800);
  await click(page, page.getByText("Refund the printer customer"));
  await page.mouse.move(1000, 660, { steps: 10 });
  await caption(page, "The receiver carried it out and answered. The agent gets the same result, and the log keeps it all");
});

if (!only || only === "mandate") await film("ui-mandate.gif", async (page) => {
  const run = "film-" + Date.now();
  await propose({ action: "tickets.close", title: "Close the keyboard ticket", idempotencyKey: run + ":1", runId: run,
    rationale: "A new keyboard was delivered to desk 12.", payloadJson: JSON.stringify({ ticket_id: "T-12", resolution: "Keyboard delivered." }) });
  await page.goto(base + "/inbox");
  await caption(page, "The agent wants to close a ticket. No mandate covers closing, so it waits for a person");
  await click(page, page.getByText("Close the keyboard ticket"));
  await caption(page, "Closing tickets is routine. Alice decides to let the agent do it on its own, within limits");
  await click(page, page.getByRole("link", { name: "Mandates" }));
  await click(page, page.getByRole("button", { name: "Grant a mandate" }));
  await caption(page, "A mandate: which agent, which actions, up to which risk, how many a month, and which section of the company's AI policy allows it");
  const dialog = page.getByRole("dialog");
  await click(page, dialog.getByText("Choose an agent"));
  await click(page, page.getByRole("option", { name: /support-bot/ }));
  await click(page, dialog.getByText("tickets.close", { exact: true }));
  await click(page, dialog.getByRole("combobox").nth(1));
  await click(page, page.getByRole("option", { name: "Medium" }));
  const budget = dialog.getByLabel(/Budget/);
  await budget.fill("");
  await type(page, budget, "20");
  await type(page, dialog.getByLabel("Policy reference"), "ACME-AI-POLICY-2026-01 §4");
  await caption(page, "A condition on the payload narrows it further; Postgres checks it again on every use");
  await type(page, dialog.getByLabel(/Condition/), 'payload.resolution != ""');
  await caption(page, "Alice ticks that she is accountable for what the agent now does on its own");
  await click(page, dialog.getByText("I am accountable for this delegation"));
  await click(page, dialog.getByRole("button", { name: "Grant mandate" }));
  await dialog.waitFor({ state: "hidden" });
  await caption(page, "Granted. The close that waited still waits: a mandate allows what is proposed from now on");
  await propose({ action: "tickets.close", title: "Close the monitor ticket", idempotencyKey: run + ":2", runId: run,
    rationale: "Facilities replaced the monitor in room B.", payloadJson: JSON.stringify({ ticket_id: "T-14", resolution: "Monitor replaced." }) });
  await caption(page, "The agent proposes another close…");
  await click(page, page.getByRole("link", { name: "Inbox" }));
  await click(page, page.getByText(/^Allowed by mandate$/).first());
  await pause(page, 800);
  await click(page, page.getByText("Close the monitor ticket"));
  await page.mouse.move(1000, 660, { steps: 10 });
  await caption(page, "…and it is carried out at once, without a person. The decision names the mandate, the policy and the budget used");
});
