// YouTrack Cloud custom import for handoffkeep. Copy this file into the
// custom import package's client.js in Administration > Integrations > Imports.
// This is a one-time, re-runnable import keyed by immutable hk source IDs.
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
const DECISIONS_PROJECT_ID = 'hk-project:/decisions';

function sourceUser(name) {
  const value = name || 'unknown';
  return {id: 'hk-actor:' + value, name: value, fullName: value, type: 'user'};
}
function enumValue(name) { return name ? {type: 'enum', name: name} : null; }
function stringValue(value) { return value === null || value === undefined || value === '' ? null : {type: 'string', value: String(value)}; }
function integerValue(value) { return value === null || value === undefined ? null : {type: 'integer', value: Number(value)}; }
function sourceProject(name) { return name || 'Unassigned'; }
function projectID(name) { return 'hk-project:' + name; }
function parseProject(info) { return String(info.id).slice('hk-project:'.length); }
function safeDate(value) { return value || undefined; }
function truncateRunes(value, max) { return Array.from(String(value || '')).slice(0, max).join(''); }
function cursorID(after, prefix) {
  if (!after) return 0;
  const text = String(after);
  const value = text.startsWith(prefix) ? text.slice(prefix.length) : text;
  const id = Number(value);
  if (!Number.isSafeInteger(id) || id < 0) throw new Error('Invalid source cursor');
  return id;
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
  }

  request(path, query) {
    const response = this.connection.getSync(path, query || {});
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
        if (this.testTasks.length) {
          const maxTaskID = this.testTasks[this.testTasks.length - 1].id;
          this.pages('comments', {project: this.testSourceProject, max_task_id: String(maxTaskID), limit: 500}, 'after_id').forEach((c) => {
            (this.commentsByTask[c.task_id] || (this.commentsByTask[c.task_id] = [])).push(c);
            this.rememberUser(c.author);
          });
        }
      }
    } else {
      // Full imports preload the complete author set and comments once.
      this.pages('comments', {limit: 500}, 'after_id').forEach((c) => {
        (this.commentsByTask[c.task_id] || (this.commentsByTask[c.task_id] = [])).push(c);
        this.rememberUser(c.author);
      });
      this.pages('tasks', {limit: 999}, 'after_id').forEach((task) => this.rememberUser(task.created_by));
      this.pages('decisions', {limit: 999}, 'after_id').forEach((d) => this.rememberUser(d.answered_by || d.lane));
    }
    this.projects = this.testProject ? (this.testSourceProject === 'DEC' ? [] : [this.testSourceProject]) : this.pages('projects', {limit: 100}, 'after');
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
    return [
      "yyyy-MM-dd'T'HH:mm:ss.SSSSSS'Z'", "yyyy-MM-dd'T'HH:mm:ss.SSSSS'Z'",
      "yyyy-MM-dd'T'HH:mm:ss.SSSS'Z'", "yyyy-MM-dd'T'HH:mm:ss.SSS'Z'",
      "yyyy-MM-dd'T'HH:mm:ss.SS'Z'", "yyyy-MM-dd'T'HH:mm:ss.S'Z'",
      "yyyy-MM-dd'T'HH:mm:ss'Z'"
    ];
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
    if (info.id !== DECISIONS_PROJECT_ID) fields.push(
      {id: 'hk-doc-key', name: FIELD.docKey, type: 'string'},
      {id: 'hk-doc-prefix', name: FIELD.docPrefix, type: 'enum'}
    );
    return {id: info.id, key: info.key, name: info.name, type: 'DEFAULT', fields: fields};
  }

  issue(task) {
    const project = task.project || '';
    const projectLabel = sourceProject(project);
    const targetProject = project ? this.projectTargets[project] : this.projectTargets['/unassigned'];
    this.rememberUser(task.created_by);
    const comments = (this.commentsByTask[task.id] || []).map((c) => {
      this.rememberUser(c.author);
      return {id: 'hk-comment:' + c.id, type: 'comment', text: c.body, author: sourceUser(c.author), created: safeDate(c.created_at)};
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
    return {id: 'hk-task:' + task.id, key: String(task.id), fields: fields};
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
    tasks.forEach((task) => {
      this.seenTaskIDs[task.id] = true;
      out.push(this.issue(task));
    });
    return out;
  }
  decisionIssues(after, top, projectInfo) {
    const seen = Object.keys(this.seenDecisionIDs).length;
    if (this.maxTasks && seen >= this.maxTasks) return [];
    const query = {limit: Math.min(Math.max(top || 100, 1), 999, this.maxTasks ? this.maxTasks - seen : 999)};
    if (after) query.after_id = String(cursorID(after, 'hk-decision:'));
    const decisions = this.testDecisions
      ? this.testDecisions.filter((d) => d.id > (after ? cursorID(after, 'hk-decision:') : 0)).slice(0, top || 100)
      : this.onePage('decisions', query);
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
        [FIELD.recommended]: stringValue(d.recommended), [FIELD.due]: d.due_at ? {type: 'date', value: new Date(d.due_at).getTime()} : null
      }};
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
    const query = {project: project, limit: Math.min(Math.max(top || 100, 1), 20)};
    let cursor = 0;
    let resumePart = 0;
    if (after) {
      const match = String(after).match(/^hk-doc:(\d+):(\d+)$/);
      if (!match) throw new Error('Invalid document cursor');
      cursor = Number(match[1]);
      resumePart = Number(match[2]) + 1;
      // Re-read the current source document when the prior import page ended
      // between its index and content parts.
      query.after_id = String(cursor - 1);
    }
    const docs = this.onePage('documents', query);
    const out = [];
    (docs || []).forEach((doc) => {
      const documentLimit = this.maxDocuments || (this.testProject ? 100 : 0);
      if (documentLimit && !this.seenDocumentIDs[doc.id] && Object.keys(this.seenDocumentIDs).length >= documentLimit) return;
      this.seenDocumentIDs[doc.id] = true;
      let articles = this.documentArticles(doc, targetProject);
      if (!Array.isArray(articles)) articles = [articles];
      if (after && resumePart > 0 && doc.id === cursor) articles = articles.slice(resumePart);
      articles.forEach((article) => { if (out.length < (top || 100)) out.push(article); });
      resumePart = 0;
    });
    return out;
  }
  documentArticles(doc, project) {
    this.rememberUser(doc.created_by);
    const prefix = String(doc.key).split('/')[0];
    const content = doc.body || '';
    const pieces = splitUtf8(content, SAFE_PART_BYTES);
    const base = {project: {id: project.id, key: project.key, name: project.name}, author: sourceUser(doc.created_by), created: safeDate(doc.created_at), updated: safeDate(doc.updated_at)};
    const title = truncateRunes('[' + prefix + '] ' + doc.key + ' [' + doc.kind + ']', 250);
    const docFields = {[FIELD.hkID]: integerValue(doc.id), [FIELD.docKey]: stringValue(doc.key), [FIELD.docPrefix]: enumValue(prefix)};
    if (pieces.length === 1) return {id: 'hk-doc:' + doc.id + ':0', key: 'hk-doc:' + doc.id + ':0', fields: Object.assign({}, base, docFields, {summary: title, content: content})};
    const indexID = 'hk-doc:' + doc.id;
    const index = {id: indexID + ':0', key: indexID + ':0', fields: Object.assign({}, base, {
      summary: title + ' (분할 문서)', content: '# ' + title + '\n\n원문이 크기 제한 때문에 ' + pieces.length + '개 문서로 나뉘었습니다.\n' +
        pieces.map((_,i) => '- Part ' + (i+1) + '/' + pieces.length + ' — ' + doc.key + ' (part ' + (i+1) + ')').join('\n')
    }, docFields)};
    return [index].concat(pieces.map((part,i) => ({
      id: 'hk-doc:' + doc.id + ':' + (i+1), key: 'hk-doc:' + doc.id + ':' + (i+1),
      fields: Object.assign({}, base, docFields, {summary: truncateRunes(title + ' / part ' + (i+1) + ' of ' + pieces.length, 250), content: part, parent: {id: indexID + ':0', type: 'article'}})
    })));
  }
  getArticleUpdates() { return []; }
  getIssueUpdates() { return []; }
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
