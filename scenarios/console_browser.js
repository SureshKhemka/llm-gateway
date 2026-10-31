// Drives the console in headless Chromium: signs in, opens every view, fails on any
// JavaScript error or CSP violation, and saves screenshots.
// Usage: node scenarios/console_browser.js <base-url> <admin-key> <screenshot-dir>
const { chromium } = require("playwright");

(async () => {
  const [base, key, dir] = process.argv.slice(2);
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const problems = [];
  page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
  page.on("console", (m) => { if (m.type() === "error") problems.push("console: " + m.text()); });
  await page.goto(base + "/console/");
  await page.fill("#key", key);
  await page.click("#login-form button");
  await page.waitForSelector("#who:not(:empty)");
  const views = ["overview", "health", "catalog", "policies", "tenants", "budgets", "requests", "costs", "evals", "audit", "playground"];
  for (const v of views) {
    await page.goto(base + "/console/#" + v);
    await page.waitForTimeout(400);
    const err = await page.$eval("#view", (el) => (el.querySelector(".error") || {}).textContent || "");
    if (err) problems.push(`${v}: ${err}`);
    if (dir) await page.screenshot({ path: `${dir}/console-${v}.png`, fullPage: true });
  }
  // XSS: a tenant ID containing markup is shown as text and never executes.
  await page.goto(base + "/console/#tenants");
  await page.waitForTimeout(400);
  const xss = await page.evaluate(() => ({ ran: window.__xss === 1, text: document.querySelector("#view").textContent.includes('<img src=x onerror="window.__xss=1">'), imgs: document.querySelectorAll("#view img").length }));
  if (xss.ran || !xss.text || xss.imgs) problems.push("xss: " + JSON.stringify(xss));
  // Request explorer: open a decision.
  await page.goto(base + "/console/#requests");
  await page.waitForSelector("tr.click");
  await page.click("tr.click");
  await page.waitForSelector("#detail h2");
  // Evals: open a run's Pareto chart.
  await page.goto(base + "/console/#evals");
  await page.waitForTimeout(400);
  if (await page.$("tr.click")) { await page.click("tr.click"); await page.waitForSelector(".chart svg"); if (dir) await page.screenshot({ path: `${dir}/console-pareto.png`, fullPage: true }); }
  // Playground: explain and run side by side.
  await page.goto(base + "/console/#playground");
  await page.waitForSelector("#pg");
  await page.click('button[value="explain"]');
  await page.waitForSelector("#out h2");
  await page.fill('input[name="models"]', "sima/qwen-4b, sima/qwen-27b");
  await page.click('button[value="run"]');
  await page.waitForSelector("#out .card");
  if (dir) await page.screenshot({ path: `${dir}/console-playground-run.png`, fullPage: true });
  await browser.close();
  if (problems.length) { console.error(problems.join("\n")); process.exit(1); }
  console.log("console OK: " + views.length + " views");
})().catch((e) => { console.error(e); process.exit(1); });
