// copresence dashboard — no framework, no build step, no network beyond the
// server that served this file.
//
// Everything is built with createElement/textContent rather than innerHTML.
// Event bodies are written by agents and can contain anything at all; the way
// to be sure none of it is ever parsed as markup is to never hand it to a
// parser.

const POLL_MS = 5000;

const $ = (id) => document.getElementById(id);
const ui = {
  live: $("live"), session: $("session"), workspace: $("workspace"), head: $("head"),
  since: $("since"), allProjects: $("all-projects"), auto: $("autorefresh"),
  refresh: $("refresh"), type: $("type"), error: $("error"), kpis: $("kpis"),
  participants: $("participants"), questions: $("questions"), decisions: $("decisions"),
  timeline: $("timeline"), chart: $("chart"), costTable: $("cost-table"),
  costTabs: $("cost-tabs"), costScope: $("cost-scope"), generated: $("generated"),
  costHint: $("cost-hint"), runChip: $("run-chip"),
  nParticipants: $("n-participants"), nQuestions: $("n-questions"),
  nDecisions: $("n-decisions"), nTimeline: $("n-timeline"),
};

let costDim = "by_run";
// The run filter narrows spend only — events carry no run id — so it lives here
// rather than in the shared control bar, and the chip says what it covers.
let runFilter = "";
let typesFilled = false;
let timer = null;

// ---------- small builders ----------

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}

function clear(node) { node.replaceChildren(); }

function empty(node, msg) {
  clear(node);
  node.append(el("div", "empty", msg));
}

// ---------- formatting ----------

const money = (n) => "$" + (n || 0).toFixed(2);

function tokens(n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
}

function ago(iso) {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "";
  const s = Math.max(0, (Date.now() - then) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

// A path is distinguished by its tail: ~/dev/copresence and ~/dev/copresence-old
// differ at the end, so elide from the left.
function shortPath(p, n = 34) {
  if (!p || p.length <= n) return p || "";
  return p.includes("/") ? "…" + p.slice(-(n - 1)) : p.slice(0, n - 1) + "…";
}

// ---------- fetch ----------

async function load() {
  ui.live.classList.add("busy");
  const q = new URLSearchParams();
  if (ui.since.value) q.set("since", ui.since.value);
  if (ui.allProjects.checked) q.set("all_projects", "1");
  if (ui.type.value) q.set("type", ui.type.value);
  if (runFilter) q.set("run", runFilter);
  q.set("limit", "120");
  try {
    const res = await fetch("api/state?" + q.toString(), { headers: { Accept: "application/json" } });
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
    render(await res.json());
    ui.error.hidden = true;
    ui.live.classList.remove("stale");
  } catch (e) {
    // Keep the last good render on screen. A dashboard that blanks itself
    // because one poll failed is worse than one that says so and waits.
    ui.error.textContent = "cannot reach the server: " + e.message;
    ui.error.hidden = false;
    ui.live.classList.add("stale");
  } finally {
    ui.live.classList.remove("busy");
  }
}

function schedule() {
  clearInterval(timer);
  if (ui.auto.checked) timer = setInterval(() => { if (!document.hidden) load(); }, POLL_MS);
}

// ---------- render ----------

function render(s) {
  document.title = `copresence · ${s.session}`;
  ui.session.textContent = s.session;
  ui.workspace.textContent = shortPath(s.workspace, 60);
  ui.workspace.title = s.workspace || "";
  ui.head.textContent = "#" + s.head;
  ui.generated.textContent = "updated " + new Date(s.generated_at).toLocaleTimeString();

  fillTypes(s.types);
  renderKPIs(s);
  renderParticipants(s.participants || []);
  renderEvents(ui.questions, s.questions || [], "no open questions");
  renderEvents(ui.decisions, s.decisions || [], "nothing decided yet");
  renderEvents(ui.timeline, s.timeline || [], "the log is empty");
  ui.nParticipants.textContent = (s.participants || []).length || "";
  ui.nQuestions.textContent = (s.questions || []).length || "";
  ui.nDecisions.textContent = (s.decisions || []).length || "";
  ui.nTimeline.textContent = (s.timeline || []).length || "";
  renderCost(s.usage || {});
}

function fillTypes(types) {
  if (typesFilled || !types) return;
  for (const t of types) ui.type.append(new Option(t, t));
  typesFilled = true;
}

function renderKPIs(s) {
  const u = s.usage || {};
  const t = u.total || {};
  const sub = (u.by_scope || []).find((r) => r.key === "subagent");
  const behind = (s.participants || []).filter((p) => p.unread > 0).length;
  const allTokens = (t.input_tokens || 0) + (t.output_tokens || 0) +
    (t.cache_read_tokens || 0) + (t.cache_write_tokens || 0);

  // Every tile carries the sentence that says what it counts. The labels are
  // short enough to be ambiguous on their own — "delegated" and "session"
  // especially — and a number nobody can interpret is worse than no tile.
  const tiles = [
    ["spend", money(t.cost_usd), `${t.calls || 0} model calls`,
      "What these agents cost, for the directories and time window selected above."],
    ["tokens", tokens(allTokens), `${tokens(t.output_tokens)} generated`,
      "Every token touched, cached or not. Most of them are cache reads, which bill at a tenth of fresh input — this number runs far ahead of the cost."],
    ["delegated", sub ? money(sub.cost_usd) : "$0.00",
      sub ? `${sub.calls} calls in subagents` : "no delegated work",
      "The share spent by agents that the main agent handed work off to. Their time looks like idle time in a transcript, so it is easy to miss entirely."],
    ["session", "#" + s.head, `${(s.questions || []).length} open · ${behind} behind`,
      "The newest event in the shared log, how many questions nobody has answered, and how many participants have unread events waiting."],
  ];
  clear(ui.kpis);
  for (const [k, v, note, help] of tiles) {
    const card = el("div", "kpi");
    card.title = help;
    card.append(el("div", "k", k), el("div", "v", v), el("div", "s", note));
    ui.kpis.append(card);
  }
  if (t.unpriced_calls > 0) {
    const card = el("div", "kpi");
    card.title = "Calls whose model has no rate in the price table. They count tokens but add $0.00, " +
      "so the total above is a floor, not the whole bill.";
    card.append(el("div", "k", "unpriced"), el("div", "v", t.unpriced_calls),
      el("div", "s", "calls with no known rate"));
    ui.kpis.append(card);
  }
}

function renderParticipants(ps) {
  if (!ps.length) {
    empty(ui.participants, "nobody has joined this session");
    return;
  }
  clear(ui.participants);
  for (const p of ps) {
    const row = el("div", "row");
    const who = el("div", "who");
    who.append(el("span", "id", p.id));
    if (p.runtime) who.append(el("span", "tag", p.runtime));
    if (p.label) who.append(el("span", "tag", p.label));
    who.append(el("span", "mono muted", ago(p.seen_at)));
    row.append(who);

    const line = el("div", "row-head");
    line.append(el("span", null, `posted ${p.posted}`));
    line.append(el("span", null, `read to #${p.read_to}`));
    line.append(el("span", p.unread > 0 ? "behind" : "caught-up",
      p.unread > 0 ? `${p.unread} unread` : "caught up"));
    if (p.spend) line.append(el("span", null, `${money(p.spend.cost_usd)} · ${p.spend.calls} calls`));
    row.append(line);
    ui.participants.append(row);
  }
}

function renderEvents(node, evs, emptyMsg) {
  if (!evs.length) {
    empty(node, emptyMsg);
    return;
  }
  clear(node);
  for (const e of evs) {
    const row = el("div", "row");
    const head = el("div", "row-head");
    head.append(el("span", "seq", "#" + e.seq));
    head.append(el("span", "type type-" + e.type, e.type));
    head.append(el("span", null, e.actor));
    if (e.subject) head.append(el("span", "subject", e.subject));
    head.append(el("span", null, ago(e.ts)));
    for (const tag of e.tags || []) head.append(el("span", "tag", "#" + tag));
    row.append(head, el("div", "row-body", e.body));
    node.append(row);
  }
}

// A run key is "<source>:<uuid>". The uuid's head is the part people quote, so
// keep that and drop the tail — the opposite of how paths are shortened.
function runID(key) {
  const i = key.indexOf(":");
  return i < 0 ? key : key.slice(i + 1);
}

function runLabel(key) {
  const id = runID(key);
  return id === key ? key : key.slice(0, key.indexOf(":") + 1) + id.slice(0, 8);
}

// A group's clock span, with the date dropped off the far end when both ends
// land on the same day.
function span(first, last) {
  const a = new Date(first), b = new Date(last);
  if (Number.isNaN(a.getTime())) return "";
  const hhmm = (d) => String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  const md = (d) => String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
  const sameDay = a.toDateString() === b.toDateString();
  return `${md(a)} ${hhmm(a)}→${sameDay ? hhmm(b) : md(b)}`;
}

// updateHint spells out what the selected breakdown actually means. "scope" and
// "run" are the project's own words for things nobody can be expected to guess,
// and a table of numbers under an unexplained label is not an answer.
function updateHint() {
  const on = ui.costTabs.querySelector("button.on");
  ui.costHint.textContent = on ? on.title : "";
}

function renderCost(u) {
  updateHint();
  ui.costScope.textContent = shortPath(u.scope || "", 46) + (u.since ? ` · last ${u.since}` : "");

  // The chip has to say "spend" out loud: the rest of the page is still showing
  // the whole session, because events carry no run id to filter on.
  if (u.run) {
    clear(ui.runChip);
    ui.runChip.append(document.createTextNode(`spend from run ${u.run.slice(0, 8)} only`));
    const x = el("button", null, "✕");
    x.type = "button";
    x.title = "clear the run filter";
    x.addEventListener("click", () => { runFilter = ""; load(); });
    ui.runChip.append(x);
    ui.runChip.hidden = false;
  } else {
    ui.runChip.hidden = true;
  }

  renderChart(u.by_day || []);
  const rows = u[costDim] || [];
  if (!rows.length) {
    empty(ui.costTable, "no usage recorded — run `copresence usage import`");
    return;
  }
  const isRun = costDim === "by_run";
  const max = Math.max(...rows.map((r) => r.cost_usd || 0), 0.0001);
  const table = el("table");
  const thead = el("thead");
  const hr = el("tr");
  const cols = isRun ? ["", "when", "calls", "in", "out", "cache", "cost"]
    : ["", "calls", "in", "out", "cache", "cost"];
  for (const h of cols) hr.append(el("th", null, h));
  thead.append(hr);

  const tbody = el("tbody");
  for (const r of rows) {
    const tr = el("tr");
    const id = isRun ? runID(r.key) : null;
    const name = el("td", null, isRun ? runLabel(r.key) : shortPath(r.key, 40));
    name.title = r.key;
    // A share bar drawn straight onto the row: the eye finds the outlier
    // faster than it reads six columns of numbers.
    const pct = Math.round(((r.cost_usd || 0) / max) * 100);
    name.style.background =
      `linear-gradient(to right, var(--accent-soft) ${pct}%, transparent ${pct}%)`;
    tr.append(name);
    if (isRun) tr.append(el("td", "when", span(r.first, r.last)));
    tr.append(el("td", null, r.calls));
    tr.append(el("td", null, tokens(r.input_tokens)));
    tr.append(el("td", null, tokens(r.output_tokens)));
    tr.append(el("td", null, tokens((r.cache_read_tokens || 0) + (r.cache_write_tokens || 0))));
    tr.append(el("td", "cost", money(r.cost_usd)));

    if (isRun && r.key !== "(unknown)") {
      tr.classList.add("pick");
      if (u.run && id.startsWith(u.run)) tr.classList.add("picked");
      // Clicking the selected run clears the filter, so the row is a toggle
      // and there is always a way back out.
      tr.addEventListener("click", () => {
        runFilter = (u.run && id.startsWith(u.run)) ? "" : id;
        load();
      });
    }
    tbody.append(tr);
  }
  table.append(thead, tbody);
  clear(ui.costTable);
  ui.costTable.append(table);
}

const SVG = "http://www.w3.org/2000/svg";
const MAX_BARS = 120;

// byCalendarDay turns the sparse rows the API returns into one entry per
// calendar day.
//
// Without this the axis is categorical: two days of work a fortnight apart draw
// as two adjacent bars, which reads as "yesterday and today". The gap is part
// of the answer to "where did the money go", so it has to be drawn.
function byCalendarDay(rows) {
  if (rows.length < 2) return rows;
  const cost = new Map(rows.map((r) => [r.key, r]));
  const day = (s) => new Date(s + "T00:00:00Z");
  const out = [];
  for (let d = day(rows[0].key), end = day(rows[rows.length - 1].key); d <= end;
    d.setUTCDate(d.getUTCDate() + 1)) {
    const key = d.toISOString().slice(0, 10);
    out.push(cost.get(key) || { key, cost_usd: 0, calls: 0 });
  }
  // A long history is truncated rather than drawn as hairlines: the recent end
  // is the one anyone is looking at.
  return out.length > MAX_BARS ? out.slice(-MAX_BARS) : out;
}

function renderChart(rows) {
  clear(ui.chart);
  const days = byCalendarDay(rows);
  if (days.length < 2) return; // a single bar is not a trend
  const w = 1000, h = 100, gap = days.length > 60 ? 0.5 : 2;
  const max = Math.max(...days.map((d) => d.cost_usd || 0), 0.0001);
  const bw = w / days.length;

  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", `0 0 ${w} ${h + 14}`);
  svg.setAttribute("preserveAspectRatio", "none");
  days.forEach((d, i) => {
    if (!d.cost_usd) return; // an empty day is the gap, not a one-pixel bar
    const bh = Math.max(1, ((d.cost_usd || 0) / max) * h);
    const rect = document.createElementNS(SVG, "rect");
    rect.setAttribute("x", (i * bw + gap / 2).toFixed(2));
    rect.setAttribute("y", (h - bh).toFixed(2));
    rect.setAttribute("width", Math.max(1, bw - gap).toFixed(2));
    rect.setAttribute("height", bh.toFixed(2));
    const title = document.createElementNS(SVG, "title");
    title.textContent = `${d.key}  ${money(d.cost_usd)}  ${d.calls} calls`;
    rect.append(title);
    svg.append(rect);
  });
  // Only the endpoints are labelled: with preserveAspectRatio="none" the text
  // would stretch with the viewBox, so anything denser turns to mush.
  const label = (x, anchor, text) => {
    const t = document.createElementNS(SVG, "text");
    t.setAttribute("x", x);
    t.setAttribute("y", h + 11);
    t.setAttribute("text-anchor", anchor);
    t.textContent = text;
    return t;
  };
  svg.append(label(0, "start", days[0].key));
  svg.append(label(w, "end", `${days[days.length - 1].key}   peak ${money(max)}`));
  ui.chart.append(svg);
}

// ---------- wiring ----------

ui.refresh.addEventListener("click", load);
ui.auto.addEventListener("change", schedule);
for (const c of [ui.since, ui.allProjects, ui.type]) c.addEventListener("change", load);
ui.costTabs.addEventListener("click", (e) => {
  const b = e.target.closest("button[data-dim]");
  if (!b) return;
  costDim = b.dataset.dim;
  for (const other of ui.costTabs.children) other.classList.toggle("on", other === b);
  updateHint(); // immediately, rather than after the round trip
  load();
});
// Coming back to a backgrounded tab should not show a five-second-old number.
document.addEventListener("visibilitychange", () => { if (!document.hidden) load(); });

load();
schedule();
