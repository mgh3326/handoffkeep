// YouTrack Cloud custom import for handoffkeep. Copy this file into the
// custom import package's client.js in Administration > Integrations > Imports.
// The initial import and continuous updates are keyed by immutable hk source IDs.
const http = require('@jetbrains/youtrack-scripting-api/http');

const FIELD = {
  hkID: 'hk ID', project: 'hk 프로젝트', state: 'hk 상태', priority: 'hk 우선순위',
  lane: '레인', kind: '종류', grade: '등급', tier: '티어', pr: 'PR', job: '작업 job',
  stalled: '멈춤', decisionID: '결정 ID', decisionState: '결정 상태',
  recommended: '권장', due: '결정 기한', linkedTask: '연결 task ID', docKey: 'hk 문서 키', docPrefix: '문서 prefix'
};
const STATE = {
  triage: '분류 대기', backlog: '대기', unstarted: '착수 전', researching: '조사 중',
  developing: '개발 중', reviewing: '검토 중', merged: '머지됨', deployed: '배포됨',
  completed: '완료됨', dropped: '종료됨', duplicate: '중복', icebox: '보류(보관)'
};
const DECISION_STATE = {open: '열림', answered: '답변됨', superseded: '대체됨', expired: '만료됨'};
const PREFIXES = ['ack','advice','advice-request','briefs','decision','design','experiment','handoff',
  'note','other','policy','reference','report','research','review','runbook','task'];
const SAFE_PART_BYTES = 240 * 1024;
// Document key prefixes not imported (operator 2026-11-09): pane screen dumps are noise for reading and
// search; they stay in hk.
const EXCLUDED_DOC_PREFIXES = ['screens'];
function documentPath(doc) { return String(doc.key).split('/'); }
function isReviewPrompt(doc) {
  const path = documentPath(doc);
  return path[0] === 'review' && path.length > 1 && path[1].startsWith('mwr-');
}
function taskReportKind(doc) {
  if (!doc.task_id) return '';
  const path = documentPath(doc);
  if (path[0] === 'reports' && path[1] && path[1].startsWith('mwr-')) return 'report';
  if (path[0] === 'reports' && path.length === 3 && path[2] === 'report.md') return 'report';
  if (path[0] === 'brief' && path.length === 3) return 'brief';
  return '';
}
function excludedDoc(doc) {
  return EXCLUDED_DOC_PREFIXES.indexOf(documentPath(doc)[0]) >= 0 || isReviewPrompt(doc) || taskReportKind(doc) === 'report';
}
function documentIssueComments(doc) {
  const behavior = taskReportKind(doc);
  if (!behavior) return [];
  if (behavior === 'brief') {
    const prefix = documentPath(doc)[0];
    const title = truncateRunes('[' + prefix + '] ' + doc.key + ' [' + doc.kind + ']', 250);
    return [{id:'hk-doc-comment:' + doc.id,body:'Article: ' + title + ' (hk document key: ' + doc.key + ')',author:doc.created_by,created_at:doc.created_at}];
  }
  const parts = splitUtf8(doc.body || '', SAFE_PART_BYTES - 1024);
  return parts.map((part,index) => {
    const heading = '# ' + doc.key + (parts.length > 1 ? ' (part ' + (index + 1) + '/' + parts.length + ')' : '') + '\n\n';
    const text = heading + part;
    if (utf8Length(text) > SAFE_PART_BYTES) throw new Error('Comment part exceeds safe byte limit for ' + doc.key);
    return {id:'hk-doc-comment:' + doc.id + (index === 0 ? '' : ':part:' + (index + 1)),body:text,author:doc.created_by,created_at:doc.created_at};
  });
}
const DECISIONS_PROJECT_ID = 'hk-project:/decisions';

// hk's write protocol copies some comments to YouTrack itself (slice b1a).
// The export marks the PG original with hk_op_id (its body is unmarked); the
// marker test only catches a remote echo. Either way the import skips it, so
// 다시 시작 never adds the comment a second time.
const HK_OP_MARKER = /<!-- hk-op:[0-9]+ -->/;
function hkWritesComment(comment) {
  return (comment.hk_op_id !== undefined && comment.hk_op_id !== null && Number(comment.hk_op_id) > 0) || HK_OP_MARKER.test(String(comment.body || ''));
}
function sourceUser(name) {
  const value = name || 'unknown';
  return {id: 'hk-actor:' + value, name: value, fullName: value, type: 'user'};
}
function enumValue(name) { return name ? {type: 'enum', name: name} : null; }
function stringValue(value) { return value === null || value === undefined || value === '' ? null : {type: 'string', value: String(value)}; }
// YouTrack Cloud's import runtime rejects a numeric value here ("fields.hk ID[0].value should be of type
// string", 2026-11-09 test import), although the API reference lists number; pass the decimal string.
function integerValue(value) { return value === null || value === undefined ? null : {type: 'integer', value: String(Number(value))}; }
function sourceProject(name) { return name || 'Unassigned'; }
function projectID(name) { return 'hk-project:' + name; }
function parseProject(info) { return String(info.id).slice('hk-project:'.length); }
// YouTrack parses the fraction with Java's SSS..., which counts milliseconds: "…:11.392663Z" became
// +392663 ms (6.5 minutes late) in the 2026-11-09 test import. Send exactly millisecond precision.
function safeDate(value) {
  if (!value) return undefined;
  const t = Date.parse(value);
  if (Number.isNaN(t)) throw new Error('Invalid source timestamp ' + value);
  return new Date(t).toISOString();
}
// YouTrack parses an article key as a number (the 4th test import failed with 'For input string:
// "doc:62245:0"'). The whole document or its index keeps the hk document id; part n of a split
// document uses 900000000 + id*10 + n, which never collides with a document id (parts are at most 9).
function articleKey(docID, part) {
  if (!part) return String(docID);
  if (part > 9) throw new Error('Document ' + docID + ' needs more than 9 parts');
  return String(900000000 + Number(docID) * 10 + part);
}
function truncateRunes(value, max) { return Array.from(String(value || '')).slice(0, max).join(''); }
// YouTrack passes the last imported document as `after`: an object with id/key, or its id string.
function afterText(after) {
  if (!after) return '';
  if (typeof after === 'object') return String(after.id || after.key || '');
  return String(after);
}
function cursorID(after, prefix) {
  if (!after) return 0;
  const text = afterText(after);
  const value = text.startsWith(prefix) ? text.slice(prefix.length) : text;
  const id = Number(value);
  if (!Number.isSafeInteger(id) || id < 0) throw new Error('Invalid source cursor');
  return id;
}
function isoTimestamp(value) {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) throw new Error('Invalid update timestamp');
  return date.toISOString();
}
function compareDocumentCursor(a, b) {
  const at = timestampCursorParts(a.after_at), bt = timestampCursorParts(b.after_at);
  if (at.seconds !== bt.seconds) return at.seconds - bt.seconds;
  if (at.fraction !== bt.fraction) return at.fraction < bt.fraction ? -1 : 1;
  return Number(a.after_id) - Number(b.after_id);
}
function timestampCursorParts(value) {
  const match = String(value).match(/^(.+?)(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$/);
  if (!match) throw new Error('Invalid source cursor timestamp');
  const seconds = Date.parse(match[1] + match[3]);
  if (Number.isNaN(seconds)) throw new Error('Invalid source cursor timestamp');
  return {seconds, fraction: (match[2] || '').padEnd(9, '0').slice(0, 9)};
}

class HandoffkeepImport {
  constructor(context) {
    const p = context.parameters;
    const configuredURL = String(p.loadValue('url') || '');
    const hashAt = configuredURL.indexOf('#');
    const fragment = hashAt < 0 ? '' : configuredURL.slice(hashAt + 1);
    this.baseURL = (hashAt < 0 ? configuredURL : configuredURL.slice(0, hashAt)).replace(/\/+$/, '');
    const options = Object.create(null);
    fragment.split('&').filter(Boolean).forEach((pair) => {
      const parts = pair.split('=');
      options[decodeURIComponent(parts[0])] = decodeURIComponent(parts.slice(1).join('='));
    });
    this.testProject = options.testProject || '';
    this.testSourceProject = options.sourceProject || 'handoffkeep';
    this.maxTasks = options.maxTasks ? Number(options.maxTasks) : 0;
    this.maxDocuments = options.maxDocuments ? Number(options.maxDocuments) : 0;
    if ((this.testProject && !/^[A-Z][A-Z0-9]{1,9}$/.test(this.testProject)) ||
        (options.maxTasks && (!Number.isInteger(this.maxTasks) || this.maxTasks < 1)) ||
        (options.maxDocuments && (!Number.isInteger(this.maxDocuments) || this.maxDocuments < 1))) {
      throw new Error('Invalid test import options in the URL fragment');
    }
    this.accessClientID = p.loadValue('login');
    // Import configuration has one password field. Store a JSON object there
    // with hkToken and accessClientSecret so neither credential is in the URL.
    let secrets;
    try { secrets = JSON.parse(p.loadValue('password') || '{}'); }
    catch (_) { throw new Error('Import password must be a JSON object with hkToken and accessClientSecret'); }
    if (!secrets || typeof secrets !== 'object' || Array.isArray(secrets)) {
      throw new Error('Import password must be a JSON object with hkToken and accessClientSecret');
    }
    this.hkToken = secrets.hkToken;
    this.accessClientSecret = secrets.accessClientSecret;
    if (!this.baseURL.startsWith('https://') || !this.accessClientID || !this.hkToken || !this.accessClientSecret) {
      throw new Error('Configure an HTTPS source URL, Access client ID, and both import credentials');
    }
    this.connection = new http.Connection(this.baseURL, p.loadValue('sslKeyName'))
      .bearerAuth(this.hkToken)
      .addHeader('CF-Access-Client-Id', this.accessClientID)
      .addHeader('CF-Access-Client-Secret', this.accessClientSecret);
    this.projects = [];
    this.projectTargets = Object.create(null);
    this.seenTaskIDs = Object.create(null);
    this.seenDecisionIDs = Object.create(null);
    this.seenDocumentIDs = Object.create(null);
    this.usersByName = Object.create(null);
    this.commentsByTask = Object.create(null);
    this.documentsByProject = Object.create(null);
    this.taskUpdateCursors = Object.create(null);
    this.decisionUpdateCursors = Object.create(null);
    this.articleUpdateCursors = Object.create(null);
  }

  request(path, query) {
    // The YouTrack scripting http module accepts only string (or string-array) query values;
    // a number such as limit=100 throws "Can not correctly handle limit query parameter".
    const params = {};
    Object.keys(query || {}).forEach((k) => {
      const v = query[k];
      if (v === undefined || v === null) return;
      params[k] = Array.isArray(v) ? v.map(String) : String(v);
    });
    const response = this.connection.getSync(path, params);
    if (!response || response.code !== 200) throw new Error('handoffkeep export request failed (' + (response ? response.code : 'no response') + ')');
    return JSON.parse(response.response);
  }
  pages(resource, initialQuery, cursorName, maxItems) {
    const out = [];
    let cursor = null;
    do {
      if (maxItems && out.length >= maxItems) break;
      const query = Object.assign({}, initialQuery || {});
      if (maxItems) query.limit = Math.min(query.limit || 100, maxItems - out.length);
      if (cursor !== null) {
        if (cursorName === 'decision') {
          query.after_at = cursor.after_at;
          query.after_id = cursor.after_id;
        } else if (cursor && typeof cursor === 'object') {
          query.after_at = cursor.after_at;
          query.after_id = cursor.after_id;
        } else query[cursorName || 'after_id'] = cursor;
      }
      const page = this.request('/v1/import/' + resource, query);
      if (!Array.isArray(page.items)) throw new Error('Malformed handoffkeep ' + resource + ' page');
      out.push.apply(out, page.items);
      cursor = page.next_cursor || null;
    } while (cursor !== null);
    return out;
  }
  onePage(resource, query) {
    const page = this.request('/v1/import/' + resource, query || {});
    if (!Array.isArray(page.items)) throw new Error('Malformed handoffkeep ' + resource + ' page');
    return page.items;
  }

  prepareToImport() {
    this.commentsByTask = Object.create(null);
    this.testTasks = null;
    this.testDecisions = null;
    if (this.testProject) {
      const testLimit = this.maxTasks || 100;
      if (this.testSourceProject === 'DEC') {
        this.testDecisions = this.pages('decisions', {limit: 999}, 'after_id', testLimit);
        this.testDecisions.forEach((d) => this.rememberUser(d.answered_by || d.lane));
      } else {
        this.testTasks = this.pages('tasks', {project: this.testSourceProject, limit: 999}, 'after_id', testLimit);
        this.testTasks.forEach((task) => this.rememberUser(task.created_by));
      }
    } else {
      // Comment bodies are fetched with each task page so a continuous run never
      // scans comments for the entire task corpus.
      this.pages('comment-authors', {limit:999}, 'after').forEach((row) => this.rememberUser(row.author));
      this.pages('tasks', {limit: 999}, 'after_id').forEach((task) => this.rememberUser(task.created_by));
      this.pages('decisions', {limit: 999}, 'after_id').forEach((d) => this.rememberUser(d.answered_by || d.lane));
    }
    this.projects = this.testProject ? (this.testSourceProject === 'DEC' ? [] : [this.testSourceProject]) : this.pages('projects', {limit: 100}, 'after');
    // YouTrack runs the import in a fresh script context: getProjects() from the wizard is not
    // called again, so the project targets issue() and the article mapping read must be rebuilt here.
    this.projectTargets = Object.create(null);
    this.getProjects();
    let documentCount = 0;
    const documentLimit = this.testProject ? (this.maxDocuments || 100) : this.maxDocuments;
    const documentProjects = this.testProject ? this.projects : this.projects.concat(['/shared', '/unassigned']);
    for (const project of documentProjects) {
      let after = null;
      while (!documentLimit || documentCount < documentLimit) {
        const query = {project: project, limit: Math.min(20, documentLimit ? documentLimit - documentCount : 20)};
        if (after !== null) query.after_id = after;
        const page = this.request('/v1/import/document-authors', query);
        if (!Array.isArray(page.items)) throw new Error('Malformed handoffkeep document-author page');
        page.items.forEach((doc) => { this.rememberUser(doc.created_by); documentCount += 1; });
        if (!page.next_cursor) break;
        after = String(page.next_cursor);
      }
      if (documentLimit && documentCount >= documentLimit) break;
    }
  }
  rememberUser(name) { if (name) this.usersByName[name] = sourceUser(name); }
  getTimestampFormats() {
    return ["yyyy-MM-dd'T'HH:mm:ss.SSS'Z'"];
  }
  getUserTimeZoneId() { return 'UTC'; }
  getServerInfo() { return {version: 'handoffkeep /v1/import', time: new Date().toISOString()}; }

  getProjects() {
    if (this.testProject) {
      const testInfo = this.testSourceProject === 'DEC'
        ? {id: DECISIONS_PROJECT_ID, key: this.testProject, name: this.testProject}
        : {id: projectID(this.testSourceProject), key: this.testProject, name: this.testProject};
      if (this.testSourceProject !== 'DEC') this.projectTargets[this.testSourceProject] = testInfo;
      return [testInfo];
    }
    // The wizard calls getProjects() on a fresh instance, before any prepareToImport(): load the hk
    // project list here too (the first full-import wizard showed only the three synthetic buckets).
    if (!this.projects.length) this.projects = this.pages('projects', {limit: 100}, 'after');
    const names = this.projects.slice();
    names.sort();
    const seen = Object.create(null);
    const result = [];
    const addProject = (id, name, label, keyBase) => {
      let key = keyBase || name.toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 10);
      if (!key) throw new Error('Cannot form a YouTrack project key from an hk project name');
      if (!keyBase) {
        if (!/^[A-Z]/.test(key)) key = 'P' + key.slice(0, 9);
        if (key.length < 2) key = key.padEnd(2, '0');
      }
      if (!/^[A-Z][A-Z0-9]{1,9}$/.test(key)) throw new Error('Cannot form a valid YouTrack project key');
      const base = key;
      let suffix = 1;
      while (seen[key]) {
        suffix += 1;
        const tail = String(suffix);
        key = base.slice(0, 10 - tail.length) + tail;
      }
      seen[key] = true;
      const info = {id: id, key: key, name: label};
      result.push(info);
      return info;
    };
    names.forEach((name) => { this.projectTargets[name] = addProject(projectID(name), name, name); });
    this.projectTargets['/shared'] = addProject('hk-project:/shared', 'shared', 'Shared documents', 'SHRDOC');
    this.projectTargets['/unassigned'] = addProject('hk-project:/unassigned', 'unassigned', 'Unassigned hk tasks', 'HK');
    addProject(DECISIONS_PROJECT_ID, 'decisions', 'Decisions', 'DEC');
    return result;
  }

  fieldsForProject() {
    return [
      {id: 'hk-id', name: FIELD.hkID, type: 'integer'},
      {id: 'hk-project', name: FIELD.project, type: 'string'},
      {id: 'hk-state', name: FIELD.state, type: 'state'},
      {id: 'hk-priority', name: FIELD.priority, type: 'integer'},
      {id: 'hk-lane', name: FIELD.lane, type: 'enum'},
      {id: 'hk-kind', name: FIELD.kind, type: 'enum'},
      {id: 'hk-grade', name: FIELD.grade, type: 'enum'},
      {id: 'hk-tier', name: FIELD.tier, type: 'enum'},
      {id: 'hk-pr', name: FIELD.pr, type: 'string'},
      {id: 'hk-job', name: FIELD.job, type: 'string'},
      {id: 'hk-stalled', name: FIELD.stalled, type: 'enum', multiValue: true}
    ];
  }
  getProject(info) {
    const fields = this.fieldsForProject();
    if (info.id === DECISIONS_PROJECT_ID) fields.push(
      {id: 'hk-decision-id', name: FIELD.decisionID, type: 'integer'},
      {id: 'hk-decision-state', name: FIELD.decisionState, type: 'enum'},
      {id: 'hk-recommended', name: FIELD.recommended, type: 'string'},
      {id: 'hk-decision-due', name: FIELD.due, type: 'date'},
      {id: 'hk-linked-task-id', name: FIELD.linkedTask, type: 'integer'}
    );
    return {id: info.id, key: info.key, name: info.name, type: 'DEFAULT', fields: fields};
  }

  issue(task) {
    const project = task.project || '';
    const projectLabel = sourceProject(project);
    const targetProject = project ? this.projectTargets[project] : this.projectTargets['/unassigned'];
    if (!targetProject) throw new Error('No YouTrack target project for hk project ' + (project || '/unassigned'));
    this.rememberUser(task.created_by);
    const comments = (this.commentsByTask[task.id] || []).map((c) => {
      this.rememberUser(c.author);
      return {id: String(c.id).startsWith('hk-doc-comment:') ? String(c.id) : 'hk-comment:' + c.id, type: 'comment', text: c.body, author: sourceUser(c.author), created: safeDate(c.created_at)};
    });
    const ref = task.refs || {};
    let grade = '', tier = '';
    const marker = String(task.title || '').match(/\(grade ([SABC]\+?), (T[0-3])\)\s*$/);
    if (marker) { grade = marker[1]; tier = marker[2]; }
    if (!grade && ref.linear) { grade = ref.linear.grade || ''; tier = ref.linear.tier || ''; }
    const fields = {
      summary: truncateRunes('#' + task.id + ' ' + task.title, 250),
      description: [
        'Original hk title: ' + task.title,
        task.body_doc ? 'Body document: ' + task.body_doc : '',
        ref.pr ? 'PR: ' + ref.pr : '', ref.job_id ? 'Job: ' + ref.job_id : '',
        task.parent_id ? 'Parent task: #' + task.parent_id : ''
      ].filter(Boolean).join('\n'),
      created: safeDate(task.created_at), updated: safeDate(task.updated_at),
      author: sourceUser(task.created_by),
      project: {id: targetProject.id, key: targetProject.key, name: targetProject.name},
      comments: comments,
      tags: this.taskTags(task),
      [FIELD.hkID]: integerValue(task.id), [FIELD.project]: stringValue(projectLabel),
      [FIELD.state]: task.state ? {type: 'state', name: STATE[task.state] || task.state} : null,
      [FIELD.priority]: integerValue(task.priority), [FIELD.lane]: enumValue(task.lane),
      [FIELD.kind]: enumValue(task.kind), [FIELD.grade]: enumValue(grade),
      [FIELD.tier]: enumValue(tier), [FIELD.pr]: stringValue(ref.pr),
      [FIELD.job]: stringValue(ref.job_id), [FIELD.stalled]: task.stalled ? [{type: 'enum', name: '멈춤'}] : []
    };
    return {id: 'hk-task:' + task.id, key: String(task.id), fields: fields,
      _hkCursor: {after_at: task.import_updated_at || task.last_activity_at || task.updated_at, after_id: task.id}};
  }
  taskTags(task) {
    const tags = [];
    const marker = String(task.title || '').match(/\(grade ([SABC]\+?), (T[0-3])\)\s*$/);
    const grade = marker ? marker[1] : (task.refs && task.refs.linear && task.refs.linear.grade);
    const tier = marker ? marker[2] : (task.refs && task.refs.linear && task.refs.linear.tier);
    [['lane', task.lane], ['kind', task.kind], ['project', task.project], ['grade', grade], ['tier', tier]].forEach(([k,v]) => {
      if (v) tags.push({id: 'hk-tag:' + k + ':' + v, name: 'hk/' + k + '/' + v});
    });
    if (task.stalled) tags.push({id: 'hk-tag:stalled', name: 'hk/stalled'});
    if (task.awaiting_decision) tags.push({id: 'hk-tag:awaiting-decision', name: 'hk/awaiting-decision'});
    return tags;
  }
  getIssues(info, after, top) {
    if (info.id === DECISIONS_PROJECT_ID) return this.decisionIssues(after, top, info);
    const name = parseProject(info);
    const seen = Object.keys(this.seenTaskIDs).length;
    if (this.maxTasks && seen >= this.maxTasks) return [];
    const pageLimit = Math.min(Math.max(top || 100, 1), 999, this.maxTasks ? this.maxTasks - seen : 999);
    const query = {project: name === '/unassigned' ? '/unassigned' : name, limit: pageLimit};
    if (after) query.after_id = String(cursorID(after, 'hk-task:'));
    const out = [];
    const tasks = this.testTasks
      ? this.testTasks.filter((task) => task.id > (after ? cursorID(after, 'hk-task:') : 0)).slice(0, pageLimit)
      : this.onePage('tasks', query);
    this.loadCommentsForTasks(tasks);
    tasks.forEach((task) => {
      this.seenTaskIDs[task.id] = true;
      out.push(this.issue(task));
    });
    return out;
  }
  loadCommentsForTasks(tasks) {
    this.commentsByTask = Object.create(null);
    if (!tasks.length) return;
    const comments = this.pages('comments', {task_ids: tasks.map((task) => String(task.id)).join(','), limit: 499}, 'after_id');
    comments.forEach((comment) => {
      if (hkWritesComment(comment)) return;
      (this.commentsByTask[comment.task_id] || (this.commentsByTask[comment.task_id] = [])).push(comment);
      this.rememberUser(comment.author);
    });
    const documents = this.pages('task-documents', {task_ids: tasks.map((task) => String(task.id)).join(','), limit: 999}, 'after_id');
    documents.forEach((doc) => {
      this.rememberUser(doc.created_by);
      documentIssueComments(doc).forEach((comment) => {
        (this.commentsByTask[doc.task_id] || (this.commentsByTask[doc.task_id] = [])).push(comment);
      });
    });
    Object.keys(this.commentsByTask).forEach((taskID) => {
      this.commentsByTask[taskID].sort((a,b) => Date.parse(a.created_at)-Date.parse(b.created_at) || String(a.id).localeCompare(String(b.id)));
    });
  }
  decisionIssues(after, top, projectInfo, suppliedDecisions) {
    const seen = Object.keys(this.seenDecisionIDs).length;
    if (this.maxTasks && seen >= this.maxTasks) return [];
    const query = {limit: Math.min(Math.max(top || 100, 1), 999, this.maxTasks ? this.maxTasks - seen : 999)};
    if (after) query.after_id = String(cursorID(after, 'hk-decision:'));
    const decisions = suppliedDecisions || (this.testDecisions
      ? this.testDecisions.filter((d) => d.id > (after ? cursorID(after, 'hk-decision:') : 0)).slice(0, top || 100)
      : this.onePage('decisions', query));
    return decisions.map((d) => {
      this.seenDecisionIDs[d.id] = true;
      this.rememberUser(d.answered_by);
      const state = DECISION_STATE[d.status] || d.status;
      return {id: 'hk-decision:' + d.id, key: String(d.id), fields: {
        summary: truncateRunes('D-' + d.id + ' ' + d.question, 250),
        description: [d.answer ? '답변: ' + d.answer : '', d.recommended ? '권장: ' + d.recommended : '', d.default_action ? '기본 동작: ' + d.default_action : ''].filter(Boolean).join('\n'),
        created: safeDate(d.created_at), updated: safeDate(d.updated_at), author: sourceUser(d.answered_by || d.lane),
        project: {id: projectInfo.id, key: projectInfo.key, name: projectInfo.name}, tags: [{id: 'hk-tag:decision', name: 'hk/decision'}],
        [FIELD.hkID]: integerValue(d.id), [FIELD.decisionID]: integerValue(d.id),
        [FIELD.linkedTask]: integerValue(d.task_id),
        [FIELD.decisionState]: enumValue(state), [FIELD.lane]: enumValue(d.lane),
        [FIELD.recommended]: stringValue(d.recommended), [FIELD.due]: d.due_at ? {type: 'date', value: safeDate(d.due_at)} : null
      }, _hkCursor: {after_at: d.updated_at, after_id: d.id}};
    }).slice(0, top || 100);
  }

  getUsers(group, skip, top) {
    const users = Object.keys(this.usersByName).sort().map((key) => this.usersByName[key]);
    return users.slice(skip || 0, (skip || 0) + (top || 100));
  }

  getArticles(info, after, top) {
    if (info.id === DECISIONS_PROJECT_ID) return [];
    const project = parseProject(info);
    const targetProject = this.projectTargets[project] || {id: projectID(project), key: project, name: project};
    const pageTop = top || 100;
    let cursor = 0;
    let resumePart = 0;
    let afterID = null;
    if (after) {
      const match = afterText(after).match(/^hk-doc:(\d+):(\d+)$/);
      if (!match) throw new Error('Invalid document cursor');
      cursor = Number(match[1]);
      resumePart = Number(match[2]) + 1;
      // Re-read the current source document when the prior import page ended
      // between its index and content parts.
      afterID = cursor - 1;
    }
    // YouTrack treats a page shorter than `top` as the end (the 2026-11-09 test import stopped at
    // 20 articles of 100), while hk serves at most 20 documents per page: fill the page from as many
    // hk pages as needed.
    const out = [];
    const documentLimit = this.maxDocuments || (this.testProject ? 100 : 0);
    for (;;) {
      const query = {project: project, limit: 20};
      if (afterID !== null) query.after_id = afterID;
      const docs = this.onePage('documents', query) || [];
      for (const doc of docs) {
        if (excludedDoc(doc)) continue;
        if (documentLimit && !this.seenDocumentIDs[doc.id] && Object.keys(this.seenDocumentIDs).length >= documentLimit) return out;
        this.seenDocumentIDs[doc.id] = true;
        let articles = this.documentArticles(doc, targetProject);
        if (!Array.isArray(articles)) articles = [articles];
        if (resumePart > 0 && doc.id === cursor) articles = articles.slice(resumePart);
        resumePart = 0;
        for (const article of articles) {
          if (out.length >= pageTop) return out;
          out.push(article);
        }
      }
      if (docs.length < 20 || out.length >= pageTop) return out;
      afterID = docs[docs.length - 1].id;
    }
  }
  documentArticles(doc, project) {
    this.rememberUser(doc.created_by);
    const prefix = String(doc.key).split('/')[0];
    const content = doc.body || '';
    const pieces = splitUtf8(content, SAFE_PART_BYTES);
    const base = {project: {id: project.id, key: project.key, name: project.name}, author: sourceUser(doc.created_by), created: safeDate(doc.created_at), updated: safeDate(doc.updated_at)};
    const title = truncateRunes('[' + prefix + '] ' + doc.key + ' [' + doc.kind + ']', 250);
    // YouTrack articles take no custom fields ("The hk ID field is not valid for this article", 2026-11-09
    // test import). The hk identity lives in the summary (full key) and the article id/key (hk-doc:<id>:<part>);
    // the prefix is a tag for filtering.
    const docFields = {tags: [{id: 'hk-tag:doc:' + prefix, name: 'hk/doc/' + prefix}]};
    if (pieces.length === 1) return {id: 'hk-doc:' + doc.id + ':0', key: articleKey(doc.id, 0), fields: Object.assign({}, base, docFields, {summary: title, content: content}), _hkCursor: {after_at: doc.updated_at, after_id: doc.id, part: 1}};
    const indexID = 'hk-doc:' + doc.id;
    const index = {id: indexID + ':0', key: articleKey(doc.id, 0), fields: Object.assign({}, base, {
      summary: title + ' (분할 문서)', content: '# ' + title + '\n\n원문이 크기 제한 때문에 ' + pieces.length + '개 문서로 나뉘었습니다.\n' +
        pieces.map((_,i) => '- Part ' + (i+1) + '/' + pieces.length + ' — ' + doc.key + ' (part ' + (i+1) + ')').join('\n')
    }, docFields), _hkCursor: {after_at: doc.updated_at, after_id: doc.id, part: 1}};
    return [index].concat(pieces.map((part,i) => ({
      id: 'hk-doc:' + doc.id + ':' + (i+1), key: articleKey(doc.id, i + 1),
      fields: Object.assign({}, base, docFields, {summary: truncateRunes(title + ' / part ' + (i+1) + ' of ' + pieces.length, 250), content: part, parent: {id: indexID + ':0', type: 'article'}}),
      _hkCursor: {after_at: doc.updated_at, after_id: doc.id, part: i + 2}
    })));
  }
  getIssueUpdates(info, after, updatedAfter, top) {
    const limit = Math.min(Math.max(top || 100, 1), 999);
    if (info.id === DECISIONS_PROJECT_ID) {
      const page = this.updatedPage('decisions', null, after, updatedAfter, limit, this.decisionUpdateCursors);
      page.rows.forEach((row) => {
        const cursor = {after_at: row.updated_at, after_id: row.id};
        this.decisionUpdateCursors['hk-decision:' + row.id] = cursor;
        this.decisionUpdateCursors[String(row.id)] = cursor;
      });
      return this.decisionIssues(null, limit, info, page.rows);
    }
    const project = parseProject(info);
    const page = this.updatedPage('tasks', project, after, updatedAfter, limit, this.taskUpdateCursors);
    const tasks = page.rows;
    if (!tasks.length) return [];
    this.loadCommentsForTasks(tasks);
    tasks.forEach((task) => {
      const cursor = {after_at: task.import_updated_at || task.last_activity_at, after_id: task.id};
      this.taskUpdateCursors['hk-task:' + task.id] = cursor;
      this.taskUpdateCursors[String(task.id)] = cursor;
    });
    return tasks.map((task) => this.issue(task));
  }
  getArticleUpdates(info, after, updatedAfter, top) {
    if (info.id === DECISIONS_PROJECT_ID) return [];
    const project = parseProject(info);
    const targetProject = this.projectTargets[project] || {id: projectID(project), key: project, name: project};
    const out = [];
    const pageTop = Math.max(1, top || 100);
    let cursor = {after_at: isoTimestamp(updatedAfter), after_id: 0};
    let resume = null;
    if (after) {
      const text = afterText(after);
      resume = after._hkCursor && after._hkCursor.before ? after._hkCursor : this.articleUpdateCursors[text];
      if (!resume) throw new Error('Unknown article update cursor ' + text);
      cursor = resume.before;
    }
    let resumeDocID = resume && resume.docID;
    let resumePart = resume && resume.part;
    let exhausted = false;
    while (out.length < pageTop && !exhausted) {
      const page = this.request('/v1/import/documents', {project: project, after_at: cursor.after_at, after_id: cursor.after_id, limit: 20});
      if (!Array.isArray(page.items)) throw new Error('Malformed handoffkeep documents page');
      let beforeDoc = cursor;
      for (const doc of page.items) {
        const docCursor = {after_at: doc.updated_at, after_id: doc.id};
        if (!excludedDoc(doc)) {
          let articles = this.documentArticles(doc, targetProject);
          if (!Array.isArray(articles)) articles = [articles];
          let firstPart = 0;
          if (resumeDocID !== null && resumeDocID !== undefined) {
            const order = compareDocumentCursor(docCursor, resume.docCursor);
            if (order === 0) firstPart = resumePart;
            else if (order < 0) { beforeDoc = docCursor; continue; }
            resumeDocID = null;
            resumePart = null;
          }
          for (let i = firstPart; i < articles.length; i += 1) {
            const article = articles[i];
            out.push(article);
            const state = {before: beforeDoc, docCursor, docID: doc.id, part: i + 1};
            this.articleUpdateCursors[article.id] = state;
            this.articleUpdateCursors[article.key] = state;
            article._hkCursor = state;
            if (out.length >= pageTop) break;
          }
        }
        beforeDoc = docCursor;
        if (out.length >= pageTop) break;
      }
      if (out.length >= pageTop) break;
      if (!page.next_cursor) exhausted = true;
      else cursor = page.next_cursor;
    }
    return out;
  }
  updatedPage(resource, project, after, updatedAfter, top, cursors) {
    let cursor = {after_at: isoTimestamp(updatedAfter), after_id: 0};
    if (after) {
      const text = afterText(after);
      cursor = after._hkCursor || cursors[text];
      if (!cursor) throw new Error('Unknown update cursor ' + text);
    }
    const query = {after_at: cursor.after_at, after_id: cursor.after_id, limit: top};
    if (project !== null) query.project = project;
    const page = this.request('/v1/import/' + resource, query);
    if (!Array.isArray(page.items)) throw new Error('Malformed handoffkeep ' + resource + ' page');
    return {rows: page.items, next: page.next_cursor};
  }
  getLinkTypes() { return []; }
}

function splitUtf8(text, maxBytes) {
  if (utf8Length(text) <= maxBytes) return [text];
  const lines = text.split(/(?<=\n)/);
  const chunks = [];
  let chunk = '';
  let chunkBytes = 0;
  lines.forEach((line) => {
    const lineBytes = utf8Length(line);
    if (lineBytes > maxBytes) {
      if (chunk) { chunks.push(chunk); chunk = ''; chunkBytes = 0; }
      let part = '';
      let partBytes = 0;
      Array.from(line).forEach((char) => {
        const charBytes = utf8Length(char);
        if (part && partBytes + charBytes > maxBytes) { chunks.push(part); part = ''; partBytes = 0; }
        part += char;
        partBytes += charBytes;
      });
      if (part) chunks.push(part);
      return;
    }
    if (chunk && chunkBytes + lineBytes > maxBytes) { chunks.push(chunk); chunk = ''; chunkBytes = 0; }
    chunk += line;
    chunkBytes += lineBytes;
  });
  if (chunk) chunks.push(chunk);
  return chunks;
}
function utf8Length(text) { return unescape(encodeURIComponent(text)).length; }

exports.Client = (context) => new HandoffkeepImport(context);
