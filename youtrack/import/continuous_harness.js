// Local proof for update-method wiring. The fixture server is separate because the
// YouTrack HTTP scripting API is synchronous; the stub uses curl against localhost.
const assert = require('node:assert/strict');
const {spawn, spawnSync, execFileSync} = require('node:child_process');
const Module = require('node:module');
const path = require('node:path');
const clientPath = path.join(__dirname, 'client.js');
const cutoff = '2026-01-02T03:04:05.000Z';
const serverSource = String.raw`
const http=require('node:http');
const old='2026-01-02T02:04:05.000Z', fresh='2026-01-02T04:04:05.000Z';
const task=(id,at,title)=>({id,lane:'demo',title,kind:'implement',state:'backlog',priority:1,refs:{},created_by:'tester',created_at:at,updated_at:at,last_activity_at:at,project:'demo',awaiting_decision:false,stalled:false});
const document=(id,at,key,body)=>({id,key,kind:'note',session:'',job:'',body,sha256:'a'.repeat(64),created_by:'tester',created_at:at,updated_at:at,task_id:2});
const data={
 tasks:[task(1,old,'older task'),task(2,fresh,'newer task')],
 comments:[{id:10,task_id:1,body:'old comment',author:'tester',created_at:old},{id:11,task_id:2,body:'new comment',author:'tester',created_at:fresh}],
 documents:[document(20,old,'demo/old','old document'),document(21,fresh,'demo/new','new document')],
 decisions:[{id:30,question:'old decision',status:'open',created_at:old,updated_at:old,lane:'demo'},{id:31,question:'new decision',status:'open',created_at:fresh,updated_at:fresh,lane:'demo'}]
};
const server=http.createServer((req,res)=>{const u=new URL(req.url,'http://localhost');const name=u.pathname.split('/').pop();const q=u.searchParams;let rows=data[name]||[];
 if(q.has('after_at')) rows=rows.filter(x=>Date.parse(name==='tasks'?x.last_activity_at:name==='documents'?x.updated_at:name==='decisions'?x.updated_at:x.created_at)>Date.parse(q.get('after_at')));
 if(q.has('project')) rows=rows.filter(x=>!x.project||x.project===q.get('project'));
 if(q.has('task_ids')){const ids=q.get('task_ids').split(',').map(Number);rows=rows.filter(x=>ids.includes(x.task_id));}
 res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({items:rows,next_cursor:null}));});
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
    const issueUpdates=client.getIssueUpdates(demo,null,cutoff,100);
    assert.deepEqual(issueUpdates.map(x=>x.id),['hk-task:2']);
    assert.equal(issueUpdates[0].fields.comments[0].text,'new comment');
    const articleUpdates=client.getArticleUpdates(demo,null,cutoff,100);
    assert.deepEqual(articleUpdates.map(x=>x.id),['hk-doc:21:0']);
    const decisions=client.getIssueUpdates({id:'hk-project:/decisions',key:'DEC',name:'Decisions'},null,cutoff,100);
    assert.deepEqual(decisions.map(x=>x.id),['hk-decision:31']);
    console.log('PASS task=hk-task:2 comment=hk-comment:11 document=hk-doc:21:0 decision=hk-decision:31; older rows excluded');
  } finally {
    Module._load = originalLoad;
    server.kill('SIGTERM');
    await new Promise((resolve) => server.once('exit', resolve));
  }
}

main().catch((error)=>{console.error(error);process.exitCode=1;});
