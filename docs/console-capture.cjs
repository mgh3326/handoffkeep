// Captures the production bundles against fictional same-origin API responses.
// First: HK_CONSOLE_CAPTURE_DIR=/tmp/hk-console-pages go test ./internal/ui -run TestConsoleRouteDocuments
// NODE_PATH=/tmp/hk-console-capture/node_modules node docs/console-capture.cjs
//
// Every page below is one view of the shell entry: Home, Work, the two
// decision pages, Deploys, the task record and the document viewer. The old
// standalone pages (fleet, grades, docs browse, palette, timeline, compose)
// no longer have a route in web/console/src/app/router, so they have no shot.
// Widths are the design-system checklist's 1440 and 390, each in both themes.
const { chromium } = require('playwright-core');
const fs = require('node:fs/promises');
const path = require('node:path');
const root = path.resolve(__dirname, '..');
const pagesDir = process.env.HK_CONSOLE_PAGES || '/tmp/hk-console-pages';
const at = '2026-11-06T12:00:00Z';
const ago = (hours) => new Date(Date.parse(at) - hours * 3_600_000).toISOString();
const ahead = (hours) => new Date(Date.parse(at) + hours * 3_600_000).toISOString();
const pr = (n) => `https://github.com/example/console-demo/pull/${n}`;

// ---- fictional fixtures -------------------------------------------------
// Task ids stay in 101-106, lanes use the example project names, and no host,
// account or address is real.

const task101 = {
  id: 101, lane: 'console-demo', project: 'console-demo', title: 'Compact list를 기본값으로 두기',
  kind: 'build', state: 'developing', priority: 1, awaiting_decision: true,
  created_by: 'fixture-operator', claimed_by: 'fixture-builder', created_at: ago(72), updated_at: ago(1),
  started_at: ago(70),
  refs: { pr: pr(42), head_sha: '0f1e2d3c4b5a69788796a5b4c3d2e1f001234567', decision_request: { id: 'fixture-decision-101', revision: 2, status: 'open' } },
  active_job: { role: 'lead', model: 'fixture-builder-1', profile: 'fictional-builder', host: 'Fixture host A', harness: 'fixture-cli', effort: 'high' },
};
const workTasks = [
  task101,
  { id: 102, lane: 'console-demo', project: 'console-demo', title: '결정 목록의 기한 경과 탭 정리', kind: 'build', state: 'reviewing', priority: 20, created_by: 'fixture-operator', created_at: ago(66), updated_at: ago(3), refs: { pr: pr(41) }, active_job: { role: 'review', model: 'fixture-reviewer-2', profile: 'fictional-reviewer', host: 'Fixture host B', harness: 'fixture-cli', effort: 'high' } },
  { id: 103, lane: 'console-demo', project: 'console-demo', title: '배포 실패 시도 롤백 문구 다듬기', kind: 'build', state: 'backlog', priority: 30, created_by: 'fixture-operator', created_at: ago(60), updated_at: ago(26), refs: { decision_request: { id: 'fixture-decision-103', revision: 1, status: 'open' } } },
  { id: 104, lane: 'fleet-status', project: 'console-demo', title: 'Host 상태 요약 타일 추가', kind: 'feature', state: 'unstarted', priority: 45, created_by: 'fixture-operator', created_at: ago(48), updated_at: ago(30), refs: {} },
  { id: 105, lane: 'console-demo', project: 'console-demo', title: '문서 뷰어의 긴 키 줄바꿈 점검', kind: 'build', state: 'triage', priority: 5, created_by: 'fixture-operator', created_at: ago(18), updated_at: ago(16), refs: {} },
  { id: 106, lane: 'console-demo', project: 'console-demo', title: '토큰 대비 점검', kind: 'chore', state: 'merged', priority: 50, created_by: 'fixture-operator', created_at: ago(90), updated_at: ago(50), refs: { closure: { type: 'merged', evidence: 'PR #40' }, pr: pr(40) } },
];

const attention = {
  generated_at: at,
  decisions: {
    count: 3, overdue: 1,
    items: [
      { source: 'request', id: 101, lane: 'console-demo', question: 'Compact list를 기본값으로 둘까요? 전체 보기보다 한 줄 요약이 먼저 보여야 합니다.', created_at: ago(30), due_at: ago(6), overdue: true },
      { source: 'lane_event', id: 202, lane: 'deploy-pipeline', question: '배포 실패를 지금 롤백할까요, 다음 시도까지 기다릴까요?', created_at: ago(2), due_at: ahead(5), overdue: false },
      { source: 'request', id: 103, lane: 'console-demo', question: '결정 목록의 기한 경과를 별도 탭으로 둘까요? 열림 탭의 개수 표시는 그대로 유지합니다.', created_at: ago(9), due_at: null, overdue: false },
    ],
  },
  tasks: {
    count: 2, awaiting_decision: 1, stalled: 1,
    items: [
      { id: 104, title: 'Host 상태 요약 타일 다듬기', lane: 'fleet-status', state: 'developing', reason: 'stalled', since: ago(4) },
      { id: 105, title: 'Answer flow 문구 검토', lane: 'console-demo', state: 'reviewing', reason: 'awaiting_decision', since: ago(6) },
    ],
  },
  deploys: {
    count: 2, events_capped: false,
    items: [
      { service: 'console-demo', task_id: 102, title: '결정 목록의 기한 경과 탭 정리', pr: pr(41), merged_at: ago(4) },
      { service: 'deploy-pipeline', task_id: 103, title: '배포 실패 시도 롤백 문구 다듬기', pr: pr(43), merged_at: ago(9) },
    ],
  },
};

const decisionRecord = {
  id: 101, source: 'task_request', source_ref: 'task:101', task_id: 101, lane: 'console-demo',
  question: 'Compact list를 기본값으로 둘까요? 전체 보기보다 한 줄 요약이 먼저 보여야 합니다.',
  due_at: ago(6), overdue: true, status: 'open', request_state: 'overdue',
  request_id: 'fixture-decision-101', revision: 2, created_at: ago(30), answered_at: null,
  superseded_by: '', answer: '',
};
const laneEventRecord = {
  id: 202, source: 'lane_event', source_ref: 'event:202', task_id: null, lane: 'deploy-pipeline',
  question: '배포 실패를 지금 롤백할까요, 다음 시도까지 기다릴까요?',
  due_at: ahead(5), overdue: false, status: 'open', request_state: 'open',
  request_id: 'fixture-request-202', revision: 1, created_at: ago(2), answered_at: null,
  superseded_by: '', answer: '',
};
const openRecord = {
  id: 103, source: 'task_request', source_ref: 'task:103', task_id: 103, lane: 'console-demo',
  question: '결정 목록의 기한 경과를 별도 탭으로 둘까요? 열림 탭의 개수 표시는 그대로 유지합니다.',
  due_at: null, overdue: false, status: 'open', request_state: 'open',
  request_id: 'fixture-decision-103', revision: 1, created_at: ago(9), answered_at: null,
  superseded_by: '', answer: '',
};
const answeredRecord = {
  ...openRecord, id: 91, request_state: 'answered', status: 'answered', answered_at: ago(20),
  answer: '별도 탭을 유지합니다.', due_at: null,
};
const uncleanedRecord = {
  ...openRecord, id: 84, request_state: 'uncleaned', status: 'open', task_id: 106, lane: 'console-demo',
  question: '머지된 태스크에 남은 요청을 지금 정리할까요?', created_at: ago(40),
};

const decisionTabs = {
  open: { generated_at: at, tab: 'open', counts: { open: 3, overdue: 1 }, truncated: false, items: [decisionRecord, laneEventRecord, openRecord] },
  overdue: { generated_at: at, tab: 'overdue', counts: { open: 3, overdue: 1 }, truncated: false, items: [decisionRecord] },
  answered: { generated_at: at, tab: 'answered', counts: { open: 3, overdue: 1 }, truncated: false, items: [answeredRecord] },
  cleanup: { generated_at: at, tab: 'cleanup', counts: { open: 3, overdue: 1 }, truncated: false, items: [uncleanedRecord] },
};

const decisionRequest = {
  id: 'fixture-decision-101', revision: 2, state: 'overdue',
  question: 'Compact list를 기본값으로 둘까요?\n\n전체 보기보다 한 줄 요약이 먼저 보여야 합니다.',
  options: [
    { key: 'keep', label: 'Compact list를 기본값으로 유지', recommended: true },
    { key: 'board', label: '전체 보기를 기본값으로 전환' },
  ],
  allow_free: true, default_action: 'compact list를 유지합니다', default_option: 'keep',
  default_trigger: '기한이 지나면', due_at: ago(6), doc: 'guide/console', refs: [pr(42)],
  human_only: true, answer_policy: 'operator_direct',
};
const decisionDetail = {
  generated_at: at,
  record: decisionRecord,
  task: { id: 101, title: task101.title, state: 'developing', lane: 'console-demo', project: 'console-demo' },
  request: decisionRequest,
  history: [decisionRequest, { ...decisionRequest, revision: 1, state: 'superseded' }],
  attempts: [],
  answerable: true,
  disabled_reason: '',
};

const record = (key, result, ref, deployedAt, failedStep, recordedAt, extra = {}) => ({
  record_key: key, result, deployed_ref: ref, deployed_at: deployedAt, failed_step: failedStep,
  recorded_at: recordedAt, source: 'record', ...extra,
});
const deployPending = {
  generated_at: at, pr_source: 'task refs', events_capped: false,
  services: [
    {
      service: 'console-demo', repo: 'example/console-demo', target: 'fixture host A',
      doc_url: '/ui/doc/deploy/console-demo', record_count: 4, invalid_count: 3, docs_capped: false,
      invalid: [
        { key: 'deploy/20261031-unknown', reasons: ['key'] },
        { key: 'deploy/20261101T090000Z-console-demo', reasons: ['schema'] },
        { key: 'deploy/20261030T120000Z-console-demo', reasons: ['deployed_at', 'schema'] },
      ],
      current: record('deploy/20261105T090000Z-console-demo', 'success', '0123456789abcdef0123456789abcdef01234567', ago(20), null, ago(20)),
      recent: [
        record('deploy/20261105T090000Z-console-demo', 'success', '0123456789abcdef0123456789abcdef01234567', ago(20), null, ago(20)),
        record('deploy/20261104T090000Z-console-demo', 'failed', '89abcdef0123456789abcdef0123456789abcdef', ago(30), 3, ago(30)),
        record('deploy/20261103T090000Z-console-demo', 'success', `sha256:${'a'.repeat(64)}`, ago(44), null, ago(44)),
        record('deploy/20261102T090000Z-console-demo', 'rolled_back', null, null, 2, ago(60)),
      ],
      merged_boundary: 'deployed_at',
      merged_since: [
        { task_id: 102, title: '결정 목록의 기한 경과 탭 정리', pr: pr(41), merged_at: ago(4) },
        { task_id: 103, title: '배포 실패 시도 롤백 문구 다듬기', pr: pr(43), merged_at: ago(9) },
      ],
    },
    {
      service: 'deploy-pipeline', repo: 'example/deploy-pipeline', target: 'fixture host B',
      doc_url: '/ui/doc/deploy/deploy-pipeline', record_count: 2, invalid_count: 0, docs_capped: false,
      current: record('deploy/20261104T210000Z-deploy-pipeline', 'success', 'pw-fixture-1', ago(15), null, ago(15)),
      recent: [
        record('deploy/20261104T210000Z-deploy-pipeline', 'success', 'pw-fixture-1', ago(15), null, ago(15)),
        record('deploy/20261104T120000Z-deploy-pipeline', 'failed', null, null, 1, ago(24), { serving_maybe_changed: true }),
      ],
      merged_boundary: 'deployed_at',
      merged_since: [],
    },
    {
      service: 'fleet-status', repo: 'example/fleet-status', doc_url: '/ui/doc/deploy/fleet-status',
      record_count: 0, invalid_count: 0, docs_capped: false, current: null, recent: [],
      merged_boundary: 'no_current',
      merged_since: [{ task_id: 104, title: 'Host 상태 요약 타일 추가', pr: pr(44), merged_at: ago(12) }],
    },
  ],
};

const taskDetail = {
  task: task101,
  events: [
    { id: 1, kind: 'transition', from: 'unstarted', to: 'developing', by: 'fixture-builder', note: '착수', at: ago(70) },
    { id: 2, kind: 'awaiting_decision', from: 'false', to: 'true', by: 'fixture-builder', note: '기본값 확인 요청', at: ago(5) },
  ],
  dwell: [{ state: 'developing', seconds: 70 * 3600, open: true }],
  documents: [
    { key: 'guide/console', kind: 'guide', origin_class: 'fixture' },
    { key: 'design/2026-11-06/console-design-system', kind: 'note', origin_class: 'fixture' },
  ],
  documents_truncated: false,
  origins: [{ provider: 'linear', namespace: 'fixture-team', display_ref: 'CON-101', url: null }],
  origins_truncated: false,
  changes: [{ id: 1, pr: pr(42), state: 'open', head_sha: '0f1e2d3c4b5a69788796a5b4c3d2e1f001234567', merged_at: null, reviews: [{ id: 1, lens: 'design', round: 1, pass: 2, family: 'fixture-family', model: 'fixture-reviewer-2', verdict: 'pass', critical: 0, high: 0, medium: 1, low: 2 }], tasks: null }],
  changes_truncated: false,
  jobs: [{ job_id: 'fixture-job-1', role: 'lead', state: 'running', host: 'Fixture host A', family: 'fixture-family', profile: 'fictional-builder', model: 'fixture-builder-1', harness: 'fixture-cli', effort: 'high', skills: [{ name: 'tdd', calls: 2 }], skills_supported: true, claimed_at: ago(70), spawned_at: ago(71), ready_at: ago(70), ended_at: null, end_reason: null, diff_stat: { files: 3, insertions: 120, deletions: 8 } }],
  jobs_truncated: false,
  participants: { task_ref: '101', coverage: 'not_collected', segments: [] },
};

const document = {
  key: 'guide/console', kind: 'guide', sha256: '0000000000000000000000000000000000000000000000000000000000000000',
  updated_at: ago(48), format: 'markdown', title: 'Operator console',
  body: '# Operator console\n\nUse the compact list to follow work. Read a decision, compare the options, then confirm the answer.\n\n## Fictional data\n\nEvery name, host and id on these pages is a fixture.\n',
};

const fixtureFor = (url) => {
  const p = url.pathname;
  if (p === '/ui/api/attention') return attention;
  if (p === '/ui/api/decisions') {
    const tab = url.searchParams.get('tab') || 'open';
    return decisionTabs[tab] || decisionTabs.open;
  }
  if (p === '/ui/api/decisions/101') return decisionDetail;
  if (p === '/ui/api/board/projects') return { projects: ['console-demo'] };
  if (p === '/ui/api/board/tasks') return { generated_at: at, states: ['triage', 'backlog', 'unstarted', 'researching', 'developing', 'reviewing', 'merged', 'deployed', 'completed', 'dropped', 'duplicate', 'icebox'], tasks: workTasks, truncated: false };
  if (p === '/ui/api/board/tasks/101') return taskDetail;
  if (p === '/ui/api/board/doc') return document;
  if (p === '/ui/api/deploy-pending') return deployPending;
  return null;
};

const assets = {
  '.css': 'text/css', '.js': 'text/javascript', '.woff2': 'font/woff2', '.svg': 'image/svg+xml',
};

// Each page is one shell view; the file name is the route's short name.
const views = {
  home: { url: '/ui', wait: '.ha-item' },
  work: { url: '/ui/work?view=all', wait: '.work-rows a' },
  task: { url: '/ui/tasks/101', wait: '.work-record h1' },
  decisions: { url: '/ui/decisions', wait: '.decision-row' },
  decision: { url: '/ui/decisions/101', wait: '.decision-page h1' },
  deploys: { url: '/ui/deploys', wait: '.dp-card' },
  document: { url: '/ui/doc/guide/console', wait: '.hk-doc h1' },
};

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true });
  const selected = new Set((process.env.HK_CONSOLE_CAPTURE_VIEWS || '').split(',').filter(Boolean));
  // The test renders the shell without a signed-in operator, so add the write
  // token the decision page reads from <meta name="hk-csrf">. It is a fixture
  // token: every POST is answered with a fictional failure below.
  const shellHtml = (await fs.readFile(path.join(pagesDir, 'shell.html'), 'utf8'))
    .replace('</head>', '<meta name="hk-csrf" content="fixture-token"></head>');
  const manifest = [];
  const shots = path.join(root, 'docs/console-shots');
  await fs.mkdir(shots, { recursive: true });
  try {
    for (const width of [1440, 390]) {
      for (const theme of ['dark', 'light']) {
        const context = await browser.newContext({ viewport: { width, height: 900 }, deviceScaleFactor: 1 });
        await context.addInitScript((value) => localStorage.setItem('hk-console:theme', value), theme);
        const page = await context.newPage();
        const errors = [];
        page.on('pageerror', (e) => errors.push(e.message));
        await page.route('**/*', async (route) => {
          const url = new URL(route.request().url());
          if (route.request().method() !== 'GET') return route.fulfill({ status: 503, body: 'Fictional write failure' });
          if (url.pathname.startsWith('/ui/static/')) {
            const file = path.join(root, 'internal/ui/static', url.pathname.slice('/ui/static/'.length));
            try {
              const body = await fs.readFile(file);
              return route.fulfill({ body, contentType: assets[path.extname(file)] || 'application/octet-stream' });
            } catch {
              return route.fulfill({ status: 404, body: 'Missing asset' });
            }
          }
          if (url.pathname === '/ui/events') return route.fulfill({ contentType: 'text/event-stream', body: '' });
          const fixture = fixtureFor(url);
          if (fixture) return route.fulfill({ json: fixture });
          if (Object.values(views).some((v) => new URL(v.url, 'http://console.example').pathname === url.pathname)) {
            return route.fulfill({ body: shellHtml, contentType: 'text/html' });
          }
          return route.fulfill({ status: 404, body: 'No fixture for ' + url.pathname });
        });
        for (const [name, view] of Object.entries(views)) {
          if (selected.size && !selected.has(name)) continue;
          errors.length = 0;
          await page.goto('http://console.example' + view.url);
          await page.locator(view.wait).first().waitFor({ timeout: 10_000 });
          await page.evaluate(() => document.fonts.ready);
          // Show the selected state of one option card on the decision page.
          if (name === 'decision') await page.locator('.decision-option').first().click();
          // One settled frame before the shot, so a transition never lands mid-paint.
          await page.waitForTimeout(150);
          // Capture the whole document in one viewport instead of stitching
          // with fullPage: a fixed bar stitched into a tall page lands in the
          // middle of the image. A viewport as tall as the page puts the fixed
          // bottom tab bar at the real bottom edge, after the padding that
          // keeps it from covering the last row.
          for (let i = 0; i < 2; i++) {
            const height = await page.evaluate(() => Math.min(4000, Math.max(900, document.documentElement.scrollHeight)));
            if (height === page.viewportSize().height) break;
            await page.setViewportSize({ width, height });
            await page.waitForTimeout(80);
          }
          const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
          if (width === 1440 && !(await page.locator('.hk-shell-nav').evaluate((el) => el.getBoundingClientRect().bottom >= innerHeight))) throw new Error('Sidebar does not reach viewport bottom');
          const file = `${name}-${theme}-${width}.png`;
          await page.screenshot({ path: path.join(shots, file) });
          manifest.push({ file, url: view.url, theme, width, overflow, errors: [...errors], captured_at: new Date().toISOString() });
        }
        await context.close();
      }
    }
    const previous = selected.size ? JSON.parse(await fs.readFile(path.join(shots, 'capture.json'), 'utf8')).views : [];
    const merged = new Map(previous.map((item) => [item.file, item]));
    for (const item of manifest) merged.set(item.file, item);
    await fs.writeFile(path.join(shots, 'capture.json'), JSON.stringify({ browser: browser.version(), fixtures: 'Fictional data; embedded production templates and committed bundles', views: [...merged.values()] }, null, 2) + '\n');
    const failures = manifest.filter((x) => x.overflow || x.errors.length);
    console.log(JSON.stringify(failures, null, 2));
    if (failures.length) process.exitCode = 1;
  } finally {
    await browser.close();
  }
})().catch((e) => { console.error(e); process.exitCode = 1; });
