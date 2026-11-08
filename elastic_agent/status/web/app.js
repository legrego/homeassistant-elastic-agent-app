"use strict";

// Paths are relative so the page works behind Home Assistant ingress, which
// serves it under /api/hassio_ingress/<token>/.
const STATUS_URL = "api/status";
const REFRESH_MS = 15000;
const GROUPS = ["Elastic Agent", "Connectivity", "Configuration", "Host"];
const STATUS_LABEL = { ok: "OK", info: "Note", warn: "Warning", error: "Problem", unknown: "Unknown" };
const MODE_LABEL = { standalone: "Standalone", fleet: "Fleet" };

const $ = (id) => document.getElementById(id);
let lastReport = null;
let lastFetched = 0;

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function statusClass(status) {
  return "s-" + (STATUS_LABEL[status] ? status : "unknown");
}

function dot(status) {
  return el("span", { class: "dot " + statusClass(status), "aria-hidden": "true" });
}

function pill(status, text) {
  return el("span", { class: "pill " + statusClass(status) }, text || STATUS_LABEL[status] || status);
}

function stateStatus(state) {
  if (state === "healthy") return "ok";
  if (state === "failed") return "error";
  return "warn";
}

function ago(date) {
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} h ${minutes % 60} min`;
  return `${Math.floor(hours / 24)} days`;
}

function renderOverall(report) {
  const problems = report.checks.filter((c) => c.status === "error" || c.status === "warn");
  const titles = {
    ok: "Everything looks good",
    warn: problems.length === 1 ? "1 thing needs attention" : `${problems.length} things need attention`,
    error: "Something's wrong",
    unknown: "Some checks couldn't run",
  };
  $("overall").hidden = false;
  $("overall").className = "overall " + statusClass(report.overall);
  $("overall-dot").className = "dot " + statusClass(report.overall);
  $("overall-title").textContent = titles[report.overall] || titles.unknown;
  $("attention").replaceChildren(
    ...problems
      .sort((a, b) => (a.status === "error" ? -1 : 0) - (b.status === "error" ? -1 : 0))
      .map((c) => el("li", {}, `${c.title}: ${c.summary}`)),
  );
}

function fact(label, ...value) {
  return el("div", { class: "fact" }, el("div", { class: "label" }, label), el("div", { class: "value" }, ...value));
}

function renderFacts(report) {
  const s = report.summary;
  const facts = [];
  if (s.mode) facts.push(fact("Mode", MODE_LABEL[s.mode] || s.mode));
  if (s.destination) facts.push(fact(s.mode === "fleet" ? "Fleet Server" : "Sending to", s.destination));
  if (s.host_name) facts.push(fact("Host", s.host_name));
  if (s.agent_version) facts.push(fact("Elastic Agent", s.agent_version));
  if (s.app_version) {
    facts.push(
      fact(
        "App version",
        s.app_version,
        s.update_available && s.latest_version ? [" ", pill("info", `${s.latest_version} available`)] : null,
      ),
    );
  }
  if (s.started_at) {
    const started = new Date(s.started_at);
    facts.push(fact("Running for", ago(started), " ", el("small", {}, `since ${started.toLocaleString()}`)));
  }
  if (s.tags && s.tags.length) facts.push(fact("Tags", s.tags.join(", ")));
  $("facts").hidden = facts.length === 0;
  $("facts").replaceChildren(...facts);
}

function renderGroups(report) {
  const groups = [...GROUPS, ...new Set(report.checks.map((c) => c.group))].filter(
    (g, i, all) => all.indexOf(g) === i,
  );
  const cards = [];
  for (const group of groups) {
    const checks = report.checks.filter((c) => c.group === group);
    if (!checks.length) continue;
    cards.push(
      el(
        "section",
        { class: "card" },
        el("h2", {}, group),
        checks.map((c) =>
          el(
            "div",
            { class: "check " + statusClass(c.status), "data-check": c.id },
            dot(c.status),
            el(
              "div",
              { class: "body" },
              el("div", {}, el("span", { class: "title" }, c.title), " ", pill(c.status)),
              el("div", {}, c.summary),
              c.detail ? el("div", { class: "detail" }, c.detail) : null,
            ),
          ),
        ),
      ),
    );
  }
  $("groups").replaceChildren(...cards);
}

function renderComponents(report) {
  const agent = report.agent;
  const card = $("components-card");
  if (!agent || !agent.running) {
    card.hidden = true;
    return;
  }
  card.hidden = false;
  if (!agent.components.length) {
    $("components").replaceChildren(el("p", { class: "empty" }, "The agent isn't running any components."));
    return;
  }
  $("components").replaceChildren(
    ...agent.components.map((c) =>
      el(
        "div",
        { class: "component" },
        el(
          "div",
          { class: "component-head" },
          dot(stateStatus(c.state)),
          el("span", { class: "name" }, c.name),
          el("code", {}, c.id),
          pill(stateStatus(c.state), c.state),
        ),
        el(
          "ul",
          { class: "units" },
          c.units.map((u) => {
            const streams = (u.streams || []).filter((s) => s.status && s.status !== "healthy");
            const msg = [u.message, ...streams.map((s) => `${s.id}: ${s.status}${s.message ? " (" + s.message + ")" : ""}`)]
              .filter((m) => m && m !== "Healthy")
              .join(" · ");
            return el(
              "li",
              {},
              el("span", { class: "kind" }, u.type),
              el("code", {}, u.id),
              pill(stateStatus(u.state), u.state),
              msg ? el("span", { class: "msg" }, msg) : null,
            );
          }),
        ),
      ),
    ),
  );
}

function renderLog(report) {
  const lines = report.startup_log || [];
  $("log-card").hidden = lines.length === 0;
  const warnings = lines.filter((l) => l.level !== "info").length;
  $("log-count").textContent = `${lines.length} lines${warnings ? `, ${warnings} warnings or errors` : ""}`;
  $("log").replaceChildren(
    ...lines.map((l) => el("span", { class: "l-" + l.level }, l.text + "\n")),
  );
}

function render(report) {
  lastReport = report;
  const mode = MODE_LABEL[report.summary.mode];
  $("subtitle").textContent = mode
    ? `${mode} mode${report.summary.host_name ? " on " + report.summary.host_name : ""}`
    : "Status";
  renderOverall(report);
  renderFacts(report);
  renderGroups(report);
  renderComponents(report);
  renderLog(report);
  updateClock();
}

function updateClock() {
  if (!lastReport) return;
  $("updated").textContent = `Checked ${ago(new Date(lastReport.generated_at))} ago`;
}

async function load(force = false) {
  const button = $("refresh");
  button.disabled = true;
  try {
    const response = await fetch(STATUS_URL + (force ? "?refresh=1" : ""), { cache: "no-store" });
    if (!response.ok) throw new Error(`The status service answered HTTP ${response.status}.`);
    render(await response.json());
    $("error").hidden = true;
  } catch (err) {
    $("error").hidden = false;
    $("error").textContent =
      `Couldn't load the status. ${err.message || err}` +
      (lastReport ? " Showing the last status." : " Is the app running?");
  } finally {
    lastFetched = Date.now();
    button.disabled = false;
  }
}

$("refresh").addEventListener("click", () => load(true));
setInterval(() => {
  updateClock();
  if (!document.hidden && Date.now() - lastFetched >= REFRESH_MS) load();
}, 1000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && Date.now() - lastFetched >= REFRESH_MS) load();
});
load();
