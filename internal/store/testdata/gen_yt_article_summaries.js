// Regenerates yt_article_summaries.jsonl, the golden of YTArticleSummary and
// YTArticleExcluded, from the import's own code (youtrack/import/client.js
// documentArticles, truncateRunes and excludedDoc). Run from the repository
// root:
//
//	node internal/store/testdata/gen_yt_article_summaries.js > internal/store/testdata/yt_article_summaries.jsonl
const fs = require('fs');
const path = require('path');
const src = fs.readFileSync(path.join(__dirname, '../../../youtrack/import/client.js'), 'utf8');
// The YouTrack scripting module is not needed by the functions read here.
const load = new Function('require', 'exports', src + '\nreturn {truncateRunes, excludedDoc};');
const {truncateRunes, excludedDoc} = load(() => ({}), {});
const docs = [
  {key: 'reports/hk4053/report-r2.md', kind: 'report', task_id: null},
  {key: 'reports/hk4053/report.md', kind: 'report', task_id: 7},
  {key: 'reports/hk4053/report.md', kind: 'report', task_id: null},
  {key: 'reports/hk4053/report-r2.md', kind: 'report', task_id: 7},
  {key: 'reports/mwr-12/x/y', kind: 'report', task_id: 7},
  {key: 'brief/2026-11-12/hk4053', kind: 'brief', task_id: 7},
  {key: 'review/mwr-319', kind: 'note', task_id: null},
  {key: 'review/other', kind: 'note', task_id: null},
  {key: 'screens/job/pane-screen.txt', kind: 'other', task_id: 7},
  {key: 'decision/2026-11-10/yt-canon-d1-d5', kind: 'note', task_id: null},
  {key: 'decision/2026-11-10/한국어-결정-문서', kind: 'note', task_id: 3},
  {key: 'plain', kind: 'other', task_id: null},
  {key: 'research/' + 'x'.repeat(300), kind: 'note', task_id: null},
  {key: 'note/' + '가'.repeat(240), kind: 'other', task_id: null},
  // The 250th code point is an astral character (two UTF-16 units).
  {key: 'note/' + 'a'.repeat(237) + '😀tail', kind: 'other', task_id: null},
];
docs.forEach((doc) => {
  const prefix = String(doc.key).split('/')[0];
  const title = truncateRunes('[' + prefix + '] ' + doc.key + ' [' + doc.kind + ']', 250);
  console.log(JSON.stringify({key: doc.key, kind: doc.kind, task_id: doc.task_id, excluded: excludedDoc(doc), single: title, index: title + ' (분할 문서)'}));
});
