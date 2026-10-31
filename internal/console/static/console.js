// LLM Gateway console. Every action is a call to the admin API with the signed-in
// admin's credential, so the console enforces exactly the API's role checks [ADM-05].
"use strict";

const $ = (s, el = document) => el.querySelector(s);
const view = $("#view");
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const usd = (v) => (v == null ? "—" : "$" + Number(v).toFixed(Math.abs(v) < 0.01 ? 6 : 4));
const pct = (v) => (v == null ? "—" : (100 * v).toFixed(1) + "%");
const key = () => sessionStorage.getItem("gw-admin-key");

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    headers: { Authorization: "Bearer " + key(), "Content-Type": "application/json", ...(opts.headers || {}) },
    body: opts.body && typeof opts.body !== "string" ? JSON.stringify(opts.body) : opts.body,
  });
  if (res.status === 401) { signOut(); throw new Error("signed out"); }
  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch { data = text; }
  if (!res.ok) throw new Error((data && data.error && data.error.message) || res.statusText);
  return data;
}

function table(rows, cols, onClick) {
  if (!rows || !rows.length) return `<p class="muted">Nothing here yet.</p>`;
  const head = cols.map((c) => `<th>${esc(c[0])}</th>`).join("");
  const body = rows.map((r, i) => `<tr ${onClick ? `class="click" data-i="${i}"` : ""}>${cols.map((c) => `<td>${c[1](r)}</td>`).join("")}</tr>`).join("");
  return `<table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table>`;
}

function bindRows(el, rows, onClick) {
  el.querySelectorAll("tr.click").forEach((tr) => tr.addEventListener("click", () => onClick(rows[+tr.dataset.i])));
}

function bar(pctUsed) {
  const p = Math.max(0, Math.min(100, pctUsed || 0));
  const cls = p >= 100 ? "over" : p >= 80 ? "hot" : "";
  return `<div class="bar ${cls}" title="${p.toFixed(1)}%"><i data-w="${p}"></i></div>`;
}

const views = {
  async overview() {
    const o = await api("/admin/v1/overview");
    view.innerHTML = `<h1>Overview</h1><div class="cards">
      ${[["Requests / s (last min)", (o.requests_per_second || 0).toFixed(2)], ["Requests today", o.requests_today || 0],
        ["Spend today", usd(o.spend_today_usd)], ["Routing savings today", usd(o.routing_savings_today_usd)],
        ["Cache savings today", usd(o.cache_savings_today_usd)], ["Cache hit rate", pct(o.cache_hit_rate)],
        ["Error rate", pct(o.error_rate)], ["Open circuits", o.open_circuits]]
        .map(([k, v]) => `<div class="card"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join("")}</div>
      <p class="muted">As of ${esc(o.as_of)}. Refreshes every 5 s.</p>`;
  },
  async health() {
    const h = await api("/admin/v1/health");
    view.innerHTML = `<h1>Provider and deployment health</h1>` + table(h.deployments, [
      ["Deployment", (d) => esc(d.deployment)], ["Provider", (d) => esc(d.provider)], ["Tier", (d) => d.tier],
      ["Health", (d) => `<span class="${d.health.health === "healthy" ? "ok" : d.health.health === "degraded" ? "warn" : "bad"}">${esc(d.health.health)}</span>`],
      ["Breaker", (d) => esc(d.health.breaker)], ["Error rate", (d) => pct(d.health.error_rate)], ["p95", (d) => (d.health.p95_ms || 0).toFixed(0) + " ms"],
      ["Observed TTFT", (d) => (d.observed_ttft_ms ? d.observed_ttft_ms.toFixed(0) + " ms" : "—")], ["In flight", (d) => d.in_flight], ["Queued", (d) => d.queued],
      ["Cold", (d) => (d.health.cold ? "yes" : "")], ["Last error", (d) => `<span class="muted">${esc(d.health.last_error)}</span>`]]);
  },
  async catalog() {
    const c = await api("/admin/v1/catalog");
    const price = (d) => { const p = (d.prices || []).slice(-1)[0] || {}; return `${usd(p.input)} / ${usd(p.output)}`; };
    view.innerHTML = `<h1>Model catalog</h1>` + table(c.deployments, [
      ["ID", (d) => esc(d.id)], ["Provider", (d) => esc(d.provider)], ["Model", (d) => esc(d.model)], ["Tier", (d) => d.tier],
      ["Capabilities", (d) => esc((d.capabilities || []).join(", "))], ["Context", (d) => d.context_window], ["Residency", (d) => esc((d.residency || []).join(","))],
      ["Price in/out per 1M", price], ["Price cards", (d) => (d.prices || []).length], ["Enabled", (d) => (d.enabled ? "yes" : "no")]]) +
      `<h2>Virtual models</h2>` + table(c.virtual_models, [["Name", (v) => esc(v.name)], ["Policy", (v) => esc(v.policy || "(scope/default)")], ["Deployments", (v) => esc((v.deployments || []).join(", ") || "all")]]) +
      `<h2>Discovered models</h2>` + table(c.discovered, [["ID", (d) => esc(d.id)], ["Approved", (d) => (d.approved ? "yes" : `<span class="warn">awaiting approval</span>`)]]);
  },
  async policies() {
    const p = await api("/admin/v1/policies");
    view.innerHTML = `<h1>Routing policies</h1>` + p.policies.map((pol) => `
      <h2>${esc(pol.name)} <span class="pill">active v${pol.active ? pol.active.version : "?"}</span></h2>
      ${table(pol.versions, [["Version", (v) => "v" + v.version], ["Source", (v) => esc(v.source)], ["Active", (v) => (v.active ? "●" : "")],
        ["Eval run", (v) => esc(v.eval_run)], ["Override", (v) => esc(v.override_reason)],
        ["", (v) => (v.active ? "" : `<button class="secondary" data-rollback="${esc(pol.name)}" data-v="${v.version}">Activate (rollback)</button>
           <button class="secondary" data-diff="${esc(pol.name)}" data-from="${pol.active ? pol.active.version : 1}" data-v="${v.version}">Diff vs active</button>`)]])}
      <pre>${esc(JSON.stringify(pol.active, null, 2))}</pre>`).join("") + `<pre id="diff" hidden></pre>`;
    view.querySelectorAll("[data-rollback]").forEach((b) => b.addEventListener("click", async () => {
      try { await api(`/admin/v1/policies/${b.dataset.rollback}/rollback`, { method: "POST", body: { version: +b.dataset.v, reason: "console" } }); route(); }
      catch (e) { alert(e.message); }
    }));
    view.querySelectorAll("[data-diff]").forEach((b) => b.addEventListener("click", async () => {
      const d = await api(`/admin/v1/policies/${b.dataset.diff}/diff?from=${b.dataset.from}&to=${b.dataset.v}`);
      const el = $("#diff"); el.hidden = false; el.textContent = JSON.stringify(d.changes, null, 2);
    }));
  },
  async tenants() {
    const [t, k] = await Promise.all([api("/admin/v1/tenants"), api("/admin/v1/keys")]);
    view.innerHTML = `<h1>Tenants and projects</h1>` + table(t.tenants, [["Tenant", (x) => esc(x.id)], ["Projects", (x) => esc((x.projects || []).join(", "))], ["Policy", (x) => esc(x.policy)], ["Managed by", (x) => esc(x.managed)]]) +
      `<h2>Virtual keys</h2>
      <form class="inline" id="newkey"><input name="tenant" placeholder="tenant" required><input name="project" placeholder="project" required><input name="name" placeholder="name"><button>Create key</button></form>
      <pre id="secret" hidden></pre>` +
      table(k.keys, [["ID", (x) => esc(x.id)], ["Prefix", (x) => `<code>${esc(x.prefix)}</code>`], ["Tenant", (x) => esc(x.tenant)], ["Project", (x) => esc(x.project)],
        ["Valid", (x) => (x.valid ? `<span class="ok">yes</span>` : `<span class="bad">no</span>`)], ["Expires", (x) => esc(x.expires_at)], ["Managed", (x) => esc(x.managed)],
        ["", (x) => (x.valid ? `<button class="secondary" data-revoke="${esc(x.id)}">Revoke</button>` : "")]]);
    $("#newkey").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = new FormData(ev.target);
      try {
        const r = await api("/admin/v1/keys", { method: "POST", body: Object.fromEntries(f) });
        const el = $("#secret"); el.hidden = false; el.textContent = "New key (shown once):\n" + r.key;
      } catch (e) { alert(e.message); }
    });
    view.querySelectorAll("[data-revoke]").forEach((b) => b.addEventListener("click", async () => {
      if (!confirm("Revoke " + b.dataset.revoke + "?")) return;
      await api(`/admin/v1/keys/${b.dataset.revoke}/revoke`, { method: "POST", body: { reason: "console" } }); route();
    }));
  },
  async budgets() {
    const b = await api("/admin/v1/budgets");
    view.innerHTML = `<h1>Budgets</h1>` + table(b.budgets, [["Budget", (x) => esc(x.budget.id)], ["Scope", (x) => esc(x.budget.scope + " " + x.budget.scope_id)],
      ["Period", (x) => esc(x.budget.period + " (" + x.budget.timezone + ")")], ["Spent", (x) => usd(x.status && x.status.spent_usd)],
      ["Limit", (x) => usd(x.status && x.status.limit_usd)], ["Use", (x) => bar(x.status && x.status.utilisation_pct)],
      ["Soft thresholds", (x) => esc((x.budget.soft_pct || []).join("%, ") + ((x.budget.soft_pct || []).length ? "%" : ""))], ["Grants", (x) => (x.grants || []).length], ["Managed", (x) => esc(x.managed)]]);
  },
  async requests() {
    const r = await api("/admin/v1/requests?limit=200");
    view.innerHTML = `<h1>Request explorer</h1>` + table(r.requests, [["Time", (x) => esc(x.ts)], ["Tenant", (x) => esc(x.tenant)], ["Requested", (x) => esc(x.requested_model)],
      ["Served", (x) => esc(x.deployment)], ["Reason", (x) => esc(x.route_reason)], ["Cache", (x) => esc(x.cache_status)], ["Tokens in/out", (x) => `${x.input_tokens}/${x.output_tokens}`],
      ["Cost", (x) => usd(x.cost_usd)], ["Status", (x) => `<span class="${x.status < 400 ? "ok" : "bad"}">${x.status}</span>`]], true) + `<div id="detail"></div>`;
    bindRows(view, r.requests, async (x) => {
      const d = await api("/admin/v1/requests/" + x.request_id);
      $("#detail").innerHTML = `<h2>Route decision ${esc(x.request_id)}</h2>` +
        table(d.decision.candidates, [["Candidate", (c) => esc(c.deployment)], ["Tier", (c) => c.tier], ["Est. cost", (c) => usd(c.est_cost_usd)], ["Est. latency", (c) => c.est_latency_ms.toFixed(0) + " ms"], ["Health", (c) => esc(c.health)]]) +
        `<h2>Attempts</h2>` + table(d.decision.attempts, [["Deployment", (a) => esc(a.deployment)], ["Kind", (a) => esc(a.kind)], ["Status", (a) => esc(a.status)], ["Error", (a) => esc(a.error)], ["Cost", (a) => usd(a.cost_usd)], ["Latency", (a) => (a.latency_ms || 0).toFixed(1) + " ms"]]) +
        `<h2>Exclusions</h2>` + table(d.decision.exclusions, [["Deployment", (e) => esc(e.deployment)], ["Reasons", (e) => esc(e.reasons.join(", "))]]) +
        `<pre>${esc(JSON.stringify(d.decision, null, 2))}</pre>`;
    });
  },
  async costs() {
    const group = sessionStorage.getItem("gw-costs-group") || "tenant,model";
    const c = await api("/admin/v1/costs?group_by=" + encodeURIComponent(group));
    view.innerHTML = `<h1>Cost reports</h1>
      <form class="inline" id="cg"><label>Group by <input name="g" value="${esc(group)}"></label><button>Apply</button>
      <a href="#" id="csv">Download CSV</a></form>` +
      table(c.rows, [...group.split(",").map((g) => [g, (r) => esc(r.group[g])]), ["Requests", (r) => r.requests], ["Cost", (r) => usd(r.cost_usd)],
        ["Baseline", (r) => usd(r.baseline_cost_usd)], ["Routing savings", (r) => usd(r.routing_savings_usd)], ["Cache savings", (r) => usd(r.cache_savings_usd)],
        ["Tokens in/out", (r) => `${r.input_tokens}/${r.output_tokens}`]]);
    $("#cg").addEventListener("submit", (ev) => { ev.preventDefault(); sessionStorage.setItem("gw-costs-group", new FormData(ev.target).get("g")); route(); });
    $("#csv").addEventListener("click", async (ev) => {
      ev.preventDefault();
      const res = await fetch("/admin/v1/costs?format=csv&group_by=" + encodeURIComponent(group), { headers: { Authorization: "Bearer " + key() } });
      const a = document.createElement("a"); a.href = URL.createObjectURL(await res.blob()); a.download = "costs.csv"; a.click();
    });
  },
  async evals() {
    const r = await api("/admin/v1/evals/runs");
    view.innerHTML = `<h1>Eval runs</h1>` + table(r.runs, [["Run", (x) => esc(x.id)], ["Suite", (x) => esc(x.suite)], ["Status", (x) => esc(x.status)], ["Started", (x) => esc(x.started)], ["Results", (x) => x.results]], true) + `<div id="run"></div>`;
    bindRows(view, r.runs, async (x) => {
      const [d, svg] = await Promise.all([api("/admin/v1/evals/runs/" + x.id), fetch(`/admin/v1/evals/runs/${x.id}/pareto.svg`, { headers: { Authorization: "Bearer " + key() } }).then((s) => s.text())]);
      const rep = d.report || { targets: [] };
      $("#run").innerHTML = `<h2>Quality versus cost</h2><div class="chart">${svg.startsWith("<svg") ? svg : ""}</div>` +
        table(rep.targets, [["Target", (t) => esc(t.target)], ["Quality", (t) => t.quality.toFixed(1)], ["95% CI", (t) => `${t.quality_ci_low.toFixed(1)}–${t.quality_ci_high.toFixed(1)}`],
          ["Cost", (t) => usd(t.cost_usd)], ["p95", (t) => t.p95_ms.toFixed(0) + " ms"], ["vs baseline", (t) => (t.vs_baseline ? `${t.vs_baseline.quality_points.toFixed(2)} pts, ${t.vs_baseline.cost_change_pct.toFixed(1)}% cost` : "")]]);
    });
  },
  async audit() {
    const a = await api("/admin/v1/audit?limit=200");
    view.innerHTML = `<h1>Audit log</h1>` + table(a.entries, [["Time", (e) => esc(e.ts)], ["Actor", (e) => esc(e.actor + " (" + e.role + ")")], ["Action", (e) => esc(e.action)],
      ["Object", (e) => esc(e.object_kind + " " + e.object_id)], ["Reason", (e) => esc(e.reason)], ["Change", (e) => `<code class="muted">${esc(JSON.stringify(e.after || {})).slice(0, 120)}</code>`]]);
  },
  async playground() {
    const t = await api("/admin/v1/tenants");
    view.innerHTML = `<h1>Playground</h1>
      <form id="pg"><div class="form inline"><label>Tenant <select name="tenant">${t.tenants.map((x) => `<option>${esc(x.id)}</option>`).join("")}</select></label>
      <label>Models (comma-separated) <input name="models" value="auto" size="40"></label></div>
      <textarea name="prompt" placeholder="Prompt">Explain step by step why the sky is blue.</textarea>
      <div class="form inline"><button name="act" value="explain" class="secondary">Explain route</button><button name="act" value="run">Run side by side</button></div></form>
      <div id="out"></div>`;
    $("#pg").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const f = new FormData(ev.target);
      const body = { tenant: f.get("tenant"), prompt: f.get("prompt"), models: f.get("models").split(",").map((s) => s.trim()).filter(Boolean) };
      const out = $("#out");
      try {
        if (ev.submitter.value === "explain") {
          const d = await api("/admin/v1/playground/explain", { method: "POST", body });
          out.innerHTML = `<h2>Route: ${esc(d.decision && d.decision.chosen)} <span class="pill">${esc(d.decision && d.decision.reason)}</span></h2>` +
            table(d.decision ? d.decision.candidates : [], [["Candidate", (c) => esc(c.deployment)], ["Tier", (c) => c.tier], ["Estimated cost", (c) => usd(c.est_cost_usd)], ["Estimated latency", (c) => c.est_latency_ms.toFixed(0) + " ms"]]) +
            `<pre>${esc(JSON.stringify(d.decision && d.decision.difficulty_signals, null, 2))}</pre>`;
        } else {
          const r = await api("/admin/v1/playground/run", { method: "POST", body });
          out.innerHTML = `<div class="grid2">${r.results.map((x) => `<div class="card"><div class="k">${esc(x.model)} → ${esc(x.served_model)} · ${usd(x.cost_usd)} · ${(x.latency_ms || 0).toFixed(0)} ms</div>
            <pre>${esc(x.error || x.answer || JSON.stringify(x.tool_calls))}</pre></div>`).join("")}</div>`;
        }
      } catch (e) { out.innerHTML = `<p class="error">${esc(e.message)}</p>`; }
    });
  },
};

let timer;
async function route() {
  clearInterval(timer);
  const name = (location.hash || "#overview").slice(1);
  document.querySelectorAll("nav a").forEach((a) => a.classList.toggle("active", a.getAttribute("href") === "#" + name));
  if (!key()) { $("#login").hidden = false; view.innerHTML = ""; return; }
  $("#login").hidden = true;
  try {
    await (views[name] || views.overview)();
    // CSP forbids inline style attributes; widths are applied through the CSSOM.
    view.querySelectorAll("[data-w]").forEach((el) => { el.style.width = el.dataset.w + "%"; });
    if (name === "overview") timer = setInterval(() => views.overview().catch(() => {}), 5000);
  } catch (e) {
    view.innerHTML = `<p class="error">${esc(e.message)}</p>`;
  }
}

function signOut() { sessionStorage.removeItem("gw-admin-key"); $("#who").textContent = ""; route(); }

$("#login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  sessionStorage.setItem("gw-admin-key", $("#key").value.trim());
  try {
    const w = await api("/admin/v1/whoami");
    $("#who").textContent = `${w.admin.id} · ${w.admin.role}${w.admin.tenant ? " · " + w.admin.tenant : ""}`;
    route();
  } catch (e) { $("#login-error").textContent = e.message; }
});
$("#logout").addEventListener("click", signOut);
window.addEventListener("hashchange", route);
if (key()) api("/admin/v1/whoami").then((w) => { $("#who").textContent = `${w.admin.id} · ${w.admin.role}`; }).catch(() => {});
route();
