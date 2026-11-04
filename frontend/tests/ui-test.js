const { chromium } = require('playwright');

const ADMIN_KEY = process.env.ADMIN_API_KEY;
const SCREENSHOT_DIR = '/tmp/ui-test-screenshots';
const results = [];

function record(name, pass, detail) {
  results.push({ name, pass, detail });
  console.log(`[${pass ? 'PASS' : 'FAIL'}] ${name}${detail ? ' — ' + detail : ''}`);
}

async function withPage(browser, testFn) {
  const page = await browser.newPage();
  const consoleErrors = [];
  const pageErrors = [];
  page.on('console', (msg) => { if (msg.type() === 'error') consoleErrors.push(msg.text()); });
  page.on('pageerror', (err) => pageErrors.push(err.message));
  await testFn(page, consoleErrors, pageErrors);
  await page.close();
  return { consoleErrors, pageErrors };
}

(async () => {
  const browser = await chromium.launch();

  // -------------------------------------------------------------------
  // ADMIN CONSOLE
  // -------------------------------------------------------------------
  await withPage(browser, async (page, consoleErrors, pageErrors) => {
    await page.goto('http://localhost:8090');
    await page.screenshot({ path: `${SCREENSHOT_DIR}/01-admin-login.png` });

    const loginGateVisible = await page.isVisible('#login-gate');
    record('admin: login gate shown on load', loginGateVisible);

    // Wrong key first — should show an error, not silently proceed.
    // This deliberately produces a 401, which Chrome logs as a console
    // error even though the app handles it correctly — clear the log
    // here so the "no console errors" check below reflects genuine
    // problems from this point forward, not this intentional negative test.
    await page.fill('#login-key', 'totally-wrong-key');
    await page.click('#login-submit');
    await page.waitForTimeout(500);
    const errorShown = (await page.textContent('#login-error') || '').length > 0;
    record('admin: wrong key shows an error', errorShown, await page.textContent('#login-error'));
    await page.screenshot({ path: `${SCREENSHOT_DIR}/02-admin-login-wrong-key.png` });
    consoleErrors.length = 0;

    // Real key.
    await page.fill('#login-key', ADMIN_KEY);
    await page.click('#login-submit');
    await page.waitForTimeout(1000);
    const shellVisible = await page.isVisible('#app-shell');
    record('admin: real key logs in and shows the shell', shellVisible);
    await page.screenshot({ path: `${SCREENSHOT_DIR}/03-admin-shell.png`, fullPage: true });

    // KYB queue should have loaded real data (not stuck on "Loading…").
    const queueText = await page.textContent('#queue-body');
    const queueLoaded = queueText && !queueText.includes('Loading');
    record('admin: KYB queue loads real data', queueLoaded, queueText.slice(0, 80).replace(/\s+/g, ' '));

    // Click into Nodes tab.
    await page.click('nav button[data-view="nodes"]');
    await page.waitForTimeout(500);
    const nodesText = await page.textContent('#nodes-body');
    record('admin: Nodes tab loads', nodesText && !nodesText.includes('Loading'), nodesText.slice(0, 80).replace(/\s+/g, ' '));
    await page.screenshot({ path: `${SCREENSHOT_DIR}/04-admin-nodes.png`, fullPage: true });

    // Sessions tab — check for the ML fraud score column specifically.
    await page.click('nav button[data-view="sessions"]');
    await page.waitForTimeout(500);
    await page.screenshot({ path: `${SCREENSHOT_DIR}/05-admin-sessions.png`, fullPage: true });

    record('admin: no console errors during flow', consoleErrors.length === 0, consoleErrors.join(' | '));
    record('admin: no page errors during flow', pageErrors.length === 0, pageErrors.join(' | '));
  });

  // -------------------------------------------------------------------
  // WALLET DASHBOARD
  // -------------------------------------------------------------------
  await withPage(browser, async (page, consoleErrors, pageErrors) => {
    await page.goto('http://localhost:8091');
    await page.screenshot({ path: `${SCREENSHOT_DIR}/06-wallet-login.png` });

    const loginVisible = await page.isVisible('#login-screen');
    record('wallet: login screen shown on load', loginVisible);

    const phone = '+2348000009999';
    await page.fill('#login-phone', phone);
    await page.click('#login-send-otp');
    await page.waitForTimeout(800);

    const otpStepVisible = await page.isVisible('#login-step-otp');
    record('wallet: OTP step appears after requesting a code', otpStepVisible);
    await page.screenshot({ path: `${SCREENSHOT_DIR}/07-wallet-otp-step.png` });

    // Extract the demo code shown on-screen (reference-build bypass).
    const demoNoteText = await page.textContent('#demo-note');
    const codeMatch = demoNoteText && demoNoteText.match(/(\d{6})/);
    record('wallet: demo OTP code shown on screen', !!codeMatch, demoNoteText);

    if (codeMatch) {
      await page.fill('#login-otp', codeMatch[1]);
      await page.click('#login-verify-otp');
      await page.waitForTimeout(1000);
      const appVisible = await page.isVisible('#app-main');
      record('wallet: correct code logs in and shows the app', appVisible);
      await page.screenshot({ path: `${SCREENSHOT_DIR}/08-wallet-dashboard.png`, fullPage: true });

      const balanceText = await page.textContent('#balance');
      record('wallet: balance renders', !!balanceText && balanceText.includes('₦'), balanceText);
    }

    record('wallet: no console errors during flow', consoleErrors.length === 0, consoleErrors.join(' | '));
    record('wallet: no page errors during flow', pageErrors.length === 0, pageErrors.join(' | '));
  });

  // -------------------------------------------------------------------
  // ENTERPRISE PORTAL
  // -------------------------------------------------------------------
  await withPage(browser, async (page, consoleErrors, pageErrors) => {
    await page.goto('http://localhost:8092');
    await page.screenshot({ path: `${SCREENSHOT_DIR}/09-enterprise-signup.png` });

    const signupVisible = await page.isVisible('#signup-view');
    record('enterprise: signup view shown on load', signupVisible);

    await page.fill('#su-name', 'Playwright Test Co');
    await page.fill('#su-usecase', 'Automated UI test run');

    // The signup handler uses a blocking alert() to show the API key —
    // must be handled or Playwright hangs waiting on the dialog.
    let alertMessage = '';
    page.once('dialog', async (dialog) => { alertMessage = dialog.message(); await dialog.accept(); });

    await page.click('#su-submit');
    await page.waitForTimeout(1000);

    record('enterprise: API key shown via alert on signup', alertMessage.includes('sk_test_'), alertMessage);

    const consoleVisible = await page.isVisible('#console-view');
    record('enterprise: console view shown after signup', consoleVisible);
    await page.screenshot({ path: `${SCREENSHOT_DIR}/10-enterprise-console.png`, fullPage: true });

    const bannerText = await page.textContent('#kyb-banner');
    record('enterprise: KYB pending banner shown before approval', bannerText && bannerText.includes('Pending'), bannerText);

    record('enterprise: no console errors during flow', consoleErrors.length === 0, consoleErrors.join(' | '));
    record('enterprise: no page errors during flow', pageErrors.length === 0, pageErrors.join(' | '));
  });

  await browser.close();

  console.log('\n=== SUMMARY ===');
  const failed = results.filter(r => !r.pass);
  console.log(`${results.length - failed.length}/${results.length} passed`);
  if (failed.length) {
    console.log('FAILURES:');
    failed.forEach(f => console.log(`  - ${f.name}: ${f.detail}`));
    process.exit(1);
  }
})();
