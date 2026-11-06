// Run against a fresh disposable backend database and Next dev on :3000.
// Install Playwright separately (or supply PLAYWRIGHT_MODULE), then run node scripts/auth-browser-check.mjs.
import { pathToFileURL } from "node:url";

const { chromium } = await import(
  process.env.PLAYWRIGHT_MODULE ? pathToFileURL(process.env.PLAYWRIGHT_MODULE).href : "playwright"
);

import assert from "node:assert/strict";

(async () => {
  const browser = await chromium.launch({
    headless: true,
    ...(process.env.PLAYWRIGHT_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_EXECUTABLE } : {}),
  });
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto(`${process.env.ARTEX_TEST_WEB || "http://127.0.0.1:3000"}/login`);
  await page.waitForURL("**/setup");
  await page.locator("#password").fill("browser-test-password-one");
  await page.locator("#confirm").fill("browser-test-password-one");
  await page.locator("button[type=submit]").click();
  await page.waitForURL("**/function/tasks");
  await page.reload();
  await page.locator("body").waitFor();
  const storage = await page.evaluate(() => ({ token: localStorage.getItem("artex_token"), cookie: document.cookie }));
  assert.equal(storage.token, null);
  assert(!storage.cookie.includes("artex_token"));
  const cookies = await context.cookies();
  assert(cookies.some((c) => c.name === "artex_token" && c.httpOnly));
  const streams = await page.evaluate(async () => {
    const test = (url) =>
      new Promise((resolve) => {
        const es = new EventSource(url, { withCredentials: true });
        const timer = setTimeout(() => {
          es.close();
          resolve(false);
        }, 10000);
        es.onopen = () => {
          clearTimeout(timer);
          es.close();
          resolve(true);
        };
        es.onerror = () => {
          clearTimeout(timer);
          es.close();
          resolve(false);
        };
      });
    return {
      same: await test("/api/logs/stream?since=0"),
      cross: await test("http://127.0.0.1:8787/api/logs/stream?since=0"),
    };
  });
  assert(streams.same && streams.cross, JSON.stringify(streams));
  const changed = await page.evaluate(async () => {
    const r = await fetch("/api/auth/change-password", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Artex-CSRF": "1" },
      body: JSON.stringify({ old_password: "browser-test-password-one", new_password: "browser-test-password-two" }),
    });
    return r.status;
  });
  assert.equal(changed, 200);
  await page.reload();
  await page.waitForURL("**/login");
  await page.locator("#password").fill("browser-test-password-two");
  await page.locator("#agree-terms").click();
  await page.locator("button[type=submit]").click();
  await page.waitForURL("**/function/tasks");
  const logout = await page.evaluate(async () => {
    const r = await fetch("/api/auth/logout", { method: "POST", headers: { "X-Artex-CSRF": "1" } });
    return r.status;
  });
  assert.equal(logout, 200);
  await page.reload();
  await page.waitForURL("**/login");
  assert(!(await context.cookies()).some((c) => c.name === "artex_token"));
  console.log(
    JSON.stringify({
      setup: true,
      refresh: true,
      httpOnly: true,
      noLocalStorageJWT: true,
      sameOriginSSE: streams.same,
      developmentCrossPortSSE: streams.cross,
      passwordRevocation: true,
      newPasswordLogin: true,
      logout: true,
    }),
  );
  await browser.close();
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
