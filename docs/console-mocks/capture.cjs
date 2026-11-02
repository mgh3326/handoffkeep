// Run with playwright-core resolvable via NODE_PATH. Uses local Chrome, no server.
const { chromium } = require('playwright-core');
const { resolve, join } = require('node:path');
const { pathToFileURL } = require('node:url');
const { mkdirSync, writeFileSync } = require('node:fs');

async function main() {
  const directory = resolve(__dirname);
  const shots = join(directory, 'shots');
  mkdirSync(shots, { recursive: true });
  const browser = await chromium.launch({
    executablePath: process.env.CHROME_PATH || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    headless: true,
  });
  const pages = ['board', 'task-detail', 'decisions', 'fleet-hosts', 'command-palette'];
  const checks = [];
  try {
    for (const width of [1280, 390]) {
      for (const theme of ['dark', 'light']) {
        for (const name of pages) {
          const page = await browser.newPage({ viewport: { width, height: width === 390 ? 844 : 900 }, deviceScaleFactor: 1 });
          const errors = [];
          page.on('pageerror', error => errors.push(error.message));
          page.on('requestfailed', request => errors.push(request.url()));
          const url = pathToFileURL(join(directory, `${name}.html`));
          url.searchParams.set('theme', theme);
          await page.goto(url.href);
          await page.evaluate(() => document.fonts.ready);
          const layout = await page.evaluate(() => ({
            width: innerWidth,
            scrollWidth: document.documentElement.scrollWidth,
            theme: document.documentElement.dataset.theme,
          }));
          if (layout.scrollWidth > width || layout.theme !== theme || errors.length) {
            throw new Error(`${name} ${width} ${theme}: ${JSON.stringify({ layout, errors })}`);
          }
          const fullPage = name !== 'command-palette';
          await page.screenshot({ path: join(shots, `${name}-${theme}-${width}.png`), fullPage, animations: 'disabled' });
          checks.push({ page: name, width, theme, fullPage, horizontalOverflow: false, scriptErrors: 0 });
          await page.close();
        }
      }
    }
    // Exercise the preview's actual navigation and focus behavior.
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    await page.goto(pathToFileURL(join(directory, 'board.html')).href);
    await page.keyboard.press('j');
    if (await page.locator('[data-row]:focus').count() !== 1) throw new Error('J did not focus a task');
    await page.keyboard.press('Control+k');
    if (!await page.locator('[data-palette] input').evaluate(input => input === document.activeElement)) throw new Error('Palette did not take focus');
    await page.locator('[data-palette] input').fill('no-such-preview-command');
    if (!await page.locator('.palette-empty').isVisible()) throw new Error('Missing empty search state');
    await page.keyboard.press('Escape');
    if (await page.locator('[data-row]:focus').count() !== 1) throw new Error('Palette did not restore focus');
    await page.locator('[data-theme-toggle]').click();
    if (await page.locator('html').getAttribute('data-theme') !== 'light') throw new Error('Theme toggle failed');
    await page.goto(pathToFileURL(join(directory, 'decisions.html')).href);
    await page.locator('[data-decision-preview] button').click();
    if (!(await page.locator('[data-answer-status]').textContent()).includes('Nothing was sent')) throw new Error('Answer preview failed');
    await page.setViewportSize({ width: 390, height: 844 });
    await page.locator('[data-menu]').click();
    if (await page.locator('[data-menu]').getAttribute('aria-expanded') !== 'true') throw new Error('Mobile menu failed');
    await page.close();
    writeFileSync(join(shots, 'capture.json'), JSON.stringify({
      browser: await browser.version(), deviceScaleFactor: 1, scheme: 'file:',
      checks, interactions: ['row keyboard focus', 'palette focus', 'empty search', 'escape focus restoration', 'theme toggle', 'local answer preview', 'mobile menu'],
    }, null, 2) + '\n');
    console.log(`Captured ${checks.length} PNGs; layout and preview interactions passed.`);
  } finally {
    await browser.close();
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
