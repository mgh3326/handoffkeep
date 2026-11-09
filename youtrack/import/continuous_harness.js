// Local proof for update-method wiring. The fixture server is separate because the
// YouTrack HTTP scripting API is synchronous; the stub uses curl against localhost.
const assert = require('node:assert/strict');
const {spawn, spawnSync, execFileSync} = require('node:child_process');
const Module = require('node:module');
const path = require('node:path');
const clientPath = path.join(__dirname, 'client.js');
const cutoff = '2026-01-02T03:04:05.000Z';
const fresh = '2026-01-02T04:04:05.000Z';
const serverSource = String.raw`
const http=require('node:http');
const old='2026-01-02T02:04:05.000Z', fresh='2026-01-02T04:04:05.000Z';
const task=(id,at,title)=>({id,lane:'demo',title,kind:'implement',state:'backlog',priority:1,refs:{},created_by:'tester',created_at:at,updated_at:at,last_activity_at:at,project:'demo',awaiting_decision:false,stalled:false});
const document=(id,at,key,body,project='demo',created_by='tester')=>({id,key,kind:'note',session:'',job:'',body,sha256:'a'.repeat(64),created_by,created_at:at,updated_at:at,task_id:2,project});
const data={
 tasks:[task(1,old,'older task'),task(2,fresh,'newer task'),task(3,'2026-01-02T04:05:05.000Z','task after moved cursor')],
 comments:[{id:10,task_id:1,body:'old comment',author:'tester',created_at:old},{id:11,task_id:2,body:'new comment',author:'tester',created_at:fresh},{id:12,task_id:2,body:'hk wrote this one to YouTrack itself',author:'tester',created_at:fresh,hk_op_id:55},{id:13,task_id:2,body:'quotes the marker <!-- hk-op:56 --> but hk does not own it',author:'tester',created_at:fresh},{id:14,task_id:2,body:'owned, hk_op_id zero is no owner',author:'tester',created_at:fresh,hk_op_id:0}],
 documents:[document(20,old,'demo/old','old document'),document(21,fresh,'demo/new','new document'),document(22,fresh,'reports/mwr-review/report.md','mergewake report','demo','svc-mergewake'),document(23,fresh,'review/mwr-98-reviewer-prompt','review prompt'),document(24,fresh,'reports/job-a/report.md','worker report'),document(25,fresh,'brief/2026-11-09/job-a','worker brief'),document(26,fresh,'reports/job-b/report.md','x'.repeat(245*1024)),document(200,'2026-01-02T04:04:05.123600Z','precision/moved','moved precision doc','precision'),document(199,'2026-01-02T04:04:05.123789Z','precision/unemitted','unemitted precision doc','precision')].concat(Array.from({length:1001},(_,i)=>{const d=document(10000+i,fresh,'bulk/doc-'+i,'bulk document','bulk');d.task_id=null;return d;})),
 decisions:[{id:30,question:'old decision',status:'open',created_at:old,updated_at:old,lane:'demo'},{id:31,question:'new decision',status:'open',created_at:fresh,updated_at:fresh,lane:'demo'}]
};
const server=http.createServer((req,res)=>{const u=new URL(req.url,'http://localhost');if(u.pathname==='/__move'){data.tasks.find(x=>x.id===2).project='elsewhere';res.writeHead(204);res.end();return;}if(u.pathname==='/__touchdoc'){data.documents.find(x=>x.id===200).updated_at='2026-01-02T04:04:05.123999Z';res.writeHead(204);res.end();return;}const name=u.pathname.split('/').pop();const q=u.searchParams;let rows=name==='task-documents'?data.documents.filter(x=>q.get('task_ids').split(',').map(Number).includes(x.task_id)):(data[name]||[]);
 if(q.has('after_at')) rows=rows.filter(x=>{const at=Date.parse(name==='tasks'?x.last_activity_at:name==='documents'?x.updated_at:name==='decisions'?x.updated_at:x.created_at);const id=Number(x.id);return at>Date.parse(q.get('after_at'))||(at===Date.parse(q.get('after_at'))&&id>Number(q.get('after_id')||0));});
 else if(q.has('after_id')) rows=rows.filter(x=>Number(x.id)>Number(q.get('after_id')));
 if(q.has('project')) rows=rows.filter(x=>x.project===q.get('project'));
 if(q.has('task_ids')){const ids=q.get('task_ids').split(',').map(Number);rows=rows.filter(x=>ids.includes(x.task_id));}
 if(name==='documents')rows.sort((a,b)=>q.has('after_at')?(a.updated_at.localeCompare(b.updated_at)||a.id-b.id):(a.id-b.id));
 const limit=Number(q.get('limit')||100),more=rows.length>limit;rows=rows.slice(0,limit);const last=rows[rows.length-1];
 const next=more&&last?{after_at:name==='tasks'?last.last_activity_at:name==='decisions'?last.updated_at:name==='documents'?last.updated_at:last.created_at,after_id:last.id}:null;
 res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({items:rows,next_cursor:next}));});
server.listen(0,'127.0.0.1',()=>console.log(server.address().port));
`;

async function main() {
  const server = spawn(process.execPath, ['-e', serverSource], {stdio: ['ignore', 'pipe', 'inherit']});
  const port = await new Promise((resolve, reject) => {
    let output = '';
    server.stdout.on('data', (chunk) => { output += chunk.toString(); const n = Number(output.trim()); if (n) resolve(n); });
    server.once('error', reject);
  });
  const originalLoad = Module._load;
  Module._load = function(request, parent, isMain) {
    if (request === '@jetbrains/youtrack-scripting-api/http') return {Connection: class {
      constructor() { this.headers = {}; }
      bearerAuth(value) { this.headers.authorization = 'Bearer ' + value; return this; }
      addHeader(key, value) { this.headers[key] = value; return this; }
      getSync(pathname, params) {
        const query = new URLSearchParams(params).toString();
        const body = execFileSync('curl', ['-fsS', 'http://127.0.0.1:' + port + pathname + '?' + query], {encoding:'utf8'});
        return {code:200,response:body};
      }
    }};
    return originalLoad.call(this, request, parent, isMain);
  };
  try {
    delete require.cache[clientPath];
    const {Client} = require(clientPath);
    const client = Client({parameters:{loadValue:(name)=>({url:'https://fixture.invalid',login:'client',password:'{"hkToken":"token","accessClientSecret":"secret"}'}[name]||'')}});
    const demo={id:'hk-project:demo',key:'demo',name:'demo'};
    client.projectTargets.demo=demo;
    const issueUpdates=client.getIssueUpdates(demo,null,cutoff,1);
    assert.deepEqual(issueUpdates.map(x=>x.id),['hk-task:2']);
    assert.equal(issueUpdates[0].fields.comments[0].text,'new comment');
    // TestImportSkipsOriginalProtocolOwnedPGComment: the owned PG comment is unmarked; ownership (hk_op_id) skips it.
    assert(!issueUpdates[0].fields.comments.some(x=>x.id==='hk-comment:12'),'protocol-owned comment was imported');
    // Only ownership skips: a PG comment that merely quotes a marker, or has no owning op, is imported.
    assert.deepEqual(issueUpdates[0].fields.comments.filter(x=>x.id.startsWith('hk-comment:')).map(x=>x.id),['hk-comment:11','hk-comment:13','hk-comment:14'],'unowned comments were skipped');
    const docComments=issueUpdates[0].fields.comments.filter(x=>x.id.startsWith('hk-doc-comment:'));
    assert.deepEqual(docComments.map(x=>x.id),['hk-doc-comment:22','hk-doc-comment:24','hk-doc-comment:25','hk-doc-comment:26','hk-doc-comment:26:part:2']);
    assert.equal(docComments[0].author.id,'hk-actor:svc-mergewake');
    assert.equal(docComments[0].created,fresh);
    assert(docComments[0].text.startsWith('# reports/mwr-review/report.md'));
    assert(issueUpdates[0].fields.comments.find(x=>x.id==='hk-doc-comment:25').text.includes('brief/2026-11-09/job-a'));
    assert(issueUpdates[0].fields.comments.every(x=>Buffer.byteLength(x.text,'utf8')<=240*1024));
    assert(issueUpdates[0].fields.comments.every(x=>!x.text.includes('review prompt')));
    const articleUpdates=client.getArticleUpdates(demo,null,cutoff,100);
    assert.deepEqual(articleUpdates.map(x=>x.id),['hk-doc:21:0','hk-doc:25:0']);
    const fullIssues=client.getIssues(demo,null,100);
    const fullTask=fullIssues.find(x=>x.id==='hk-task:2');
    assert.deepEqual(fullTask.fields.comments.map(x=>x.id),issueUpdates[0].fields.comments.map(x=>x.id));
    const fullArticles=client.getArticles(demo,null,100);
    assert(fullArticles.some(x=>x.id==='hk-doc:25:0'));
    assert(fullArticles.every(x=>!x.id.startsWith('hk-doc:22:')&&!x.id.startsWith('hk-doc:23:')&&!x.id.startsWith('hk-doc:24:')&&!x.id.startsWith('hk-doc:26:')));
    execFileSync('curl',['-fsS','-X','POST','http://127.0.0.1:'+port+'/__move']);
    const resumedTaskClient=Client({parameters:{loadValue:(name)=>({url:'https://fixture.invalid',login:'client',password:'{"hkToken":"token","accessClientSecret":"secret"}'}[name]||'')}});
    resumedTaskClient.projectTargets.demo=demo;
    const afterMove=resumedTaskClient.getIssueUpdates(demo,issueUpdates[0],cutoff,100);
    assert.deepEqual(afterMove.map(x=>x.id),['hk-task:3']);
    const precisionInfo={id:'hk-project:precision',key:'precision',name:'precision'};
    const firstPrecision=client.getArticleUpdates(precisionInfo,null,cutoff,1);
    assert.deepEqual(firstPrecision.map(x=>x.id),['hk-doc:200:0']);
    execFileSync('curl',['-fsS','-X','POST','http://127.0.0.1:'+port+'/__touchdoc']);
    const precisionClient=Client({parameters:{loadValue:(name)=>({url:'https://fixture.invalid',login:'client',password:'{"hkToken":"token","accessClientSecret":"secret"}'}[name]||'')}});
    const resumedPrecision=precisionClient.getArticleUpdates(precisionInfo,firstPrecision[0],cutoff,10);
    assert(resumedPrecision.some(x=>x.id==='hk-doc:199:0'));
    const decisions=client.getIssueUpdates({id:'hk-project:/decisions',key:'DEC',name:'Decisions'},null,cutoff,100);
    assert.deepEqual(decisions.map(x=>x.id),['hk-decision:31']);
    const bulkInfo={id:'hk-project:bulk',key:'bulk',name:'bulk'};
    client.projectTargets.bulk=bulkInfo;
    let after=null, count=0, pageClient=client;
    for (;;) { const page=pageClient.getArticleUpdates(bulkInfo,after,cutoff,100);if(!page.length)break;count+=page.length;after=page[page.length-1];pageClient=Client({parameters:{loadValue:(name)=>({url:'https://fixture.invalid',login:'client',password:'{"hkToken":"token","accessClientSecret":"secret"}'}[name]||'')}});pageClient.projectTargets.bulk=bulkInfo; }
    assert.equal(count,1001);
    console.log('PASS task/comment/document/decision updates; protocol-owned comments skipped, unowned marker quotes kept; linked reports became stable comments, reviewer prompt stayed hk, brief remained article with backlink; fresh-client and microsecond cursors resumed; 1001 bulk articles paged; older rows excluded');
  } finally {
    Module._load = originalLoad;
    server.kill('SIGTERM');
    await new Promise((resolve) => server.once('exit', resolve));
  }
}

main().catch((error)=>{console.error(error);process.exitCode=1;});
