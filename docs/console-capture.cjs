// Captures the production bundles against fictional same-origin API responses.
// First: HK_CONSOLE_CAPTURE_DIR=/tmp/hk-console-pages go test ./internal/ui -run TestConsoleRouteDocuments
// NODE_PATH=/tmp/hk-console-capture/node_modules node docs/console-capture.cjs
const { chromium } = require('playwright-core');
const fs = require('node:fs/promises');
const path = require('node:path');
const root = path.resolve(__dirname,'..');
const at = '2026-11-01T12:00:00Z';
const task = {id:101,lane:'example-project',title:'Ship a clear operator console',kind:'build',state:'in_progress',priority:70,created_by:'example-operator',claimed_by:'example-builder',created_at:at,updated_at:at,body_doc:'guide/console',refs:{pr:'https://github.com/example/project/pull/42'}};
const tasks = [task,{...task,id:102,title:'Review the answer flow',state:'verifying',priority:60,refs:{}},{...task,id:103,title:'Choose the compact list default',state:'needs_decision',priority:50,refs:{}}];
task.work_board = {primary_job_id:'example-job',pr:'https://github.com/example/project/pull/42',head_sha:'example-head',sessions:[
 {job_id:'example-job',host:'Example host A',family:'openai',model:'Example builder model',role:'lead',active:true,at},
 {job_id:'example-review',host:'Example host B',family:'anthropic',model:'Example reviewer model',role:'review',active:true,at}
]};
const live = {generated_at:at,jobs:{status:'ok',fetched_at:at,items:[{machine:'Example host A',job_id:'example-job',role:'lead',owner_lane:'example-builder',started_at:at}]},nodes:{status:'ok',fetched_at:at,items:[{machine_id:'Example host A',state:'connected',accepting_effective:true,last_ping_ms:1000,load:{load1:2,load5:1.8,load15:1.5,ncpu:8},memory:{free_pct:65,compressed_mb:0,swap_used_mb:0,psi_some_avg10:0,source:'memory_pressure'},session_snapshot:null,display_state:'unavailable',active_jobs:1}]},links:[{task_id:101,job_id:'example-job',job_found:true,children:[]}],mismatch:{basis:'current',tasks_without_job:[],jobs_without_task:[]}};
const dashboard={generated_at:at,quota:[{account_key:'example:fictional',pool:'example',auto:{measured_at:at,buckets:[{window:'5h',used_pct:32,scope:'shared',elapsed_pct:45,pace:0.71,resets_at:'2026-11-01T17:00:00Z'}]},manual:[]}],work:tasks,prs:[{repo:'example/project',open:[{pr:task.refs.pr,number:42,task_id:101,title:task.title,state:'in_progress',at}],merged:[]}],missing:[{section:'power',source:'host power',note:'Not collected'}]};
const fixtures = {
 '/ui/api/board/tasks':{generated_at:at,states:['backlog','in_progress','verifying','needs_decision'],tasks,truncated:false},
 '/ui/api/board/tasks/101':{task,events:[{id:1,from:'claimed',to:'in_progress',by:'example-builder',at,note:'Implementation started'}],dwell:[],participants:{task_ref:'101',coverage:'not_collected',segments:[]},decision_requests:[]},
 '/ui/api/board/tasks/101/comments':{comments:[],truncated:false},
 '/ui/api/board/doc':{key:'guide/console',format:'markdown',bytes:126,sha256:'0000000000000000000000000000000000000000000000000000000000000000',body:'# Operator console\n\nUse the compact list to follow work. Read decisions carefully before confirming an answer.',title:'Operator console',kind:'guide',session:'example-project',updated_at:at},
 '/ui/api/live':live,'/ui/api/fleet':{sessions:[]},'/ui/api/dashboard':dashboard,
 '/ui/api/glance':{tasks:{decisions_pending:1,dispositions_open:0}},
 '/ui/api/docs/prefixes':{prefixes:[{prefix:'guide/',count:1}]},
 '/ui/api/docs':{prefix:'',limit:100,documents:[{key:'guide/console',title:'Operator console',kind:'guide',session:'example-project',size:126,updated_at:at}],has_more:false},
 '/ui/api/deploy-pending':{generated_at:at,pr_source:'task refs',services:[{service:'Example console',repo:'example/project',doc_url:'/ui/docs?key=guide/console',record_count:1,current:{record_key:'deploy/example',result:'success',deployed_ref:'example-release',deployed_at:at,failed_step:null,recorded_at:at,source:'record'},merged_boundary:'deployed_at',merged_since:[{task_id:102,title:'Review the answer flow',pr:'https://github.com/example/project/pull/41',merged_at:at}]}]},
 '/ui/api/search':{query:'console',scope:'tasks',results:[{...task,snippet:'Operator console'}],has_more:false,limit:20},
 '/ui/api/reap':{generated_at:at,status:'ok',fetched_at:at,candidates:[],held:[],nodes:[]},
 '/ui/api/bench/catalog':{generated_at:at,catalog:[{profile:'example-builder',effort:'high',model_id:'Example model',pool:'Example pool',grade:'A',score:91,gate:'pass',gate_reason:null,benchmark_source:'Fictional review',benchmark_annotation:null,boundary_version:'example',deviation_ref:'',decided_at:at,decided_by:'Example operator',retired_at:null}]},
};
(async()=>{
 const browser=await chromium.launch({executablePath:process.env.CHROME_PATH || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',headless:true});
 const selected=new Set((process.env.HK_CONSOLE_CAPTURE_VIEWS || '').split(',').filter(Boolean));
 const manifest=[]; const shots=path.join(root,'docs/console-shots'); await fs.mkdir(shots,{recursive:true});
 try {
 for (const width of [1280,390]) for (const theme of ['dark','light']) {
 const context=await browser.newContext({viewport:{width,height:900},deviceScaleFactor:1});
 await context.addInitScript(theme=>localStorage.setItem('hk-console:theme',theme),theme);
 const page=await context.newPage(); let writes=0; const errors=[]; page.on('pageerror',e=>errors.push(e.message));
 await page.route('**/*',async route=>{
  const url=new URL(route.request().url()); const pathname=url.pathname;
  if(route.request().method()==='POST') {writes++;return route.fulfill({status:503,body:'Fictional write failure'});}
  if(pathname.startsWith('/ui/static/')) {const file=path.join(root,'internal/ui/static',pathname.slice('/ui/static/'.length)); try{const body=await fs.readFile(file);return route.fulfill({body,contentType:file.endsWith('.css')?'text/css':'text/javascript'});}catch{return route.fulfill({status:404,body:'Missing asset'});} }
  if(fixtures[pathname]) return route.fulfill({json:fixtures[pathname]});
  if(pathname==='/ui/events') return route.fulfill({contentType:'text/event-stream',body:''});
  const names={'/ui/queue':'board','/ui/home':'inbox','/ui/fleet':'fleet','/ui/docs':'docs','/ui/deploys':'deploys','/ui/grades':'grades','/ui/decisions':'decisions','/ui/timeline':'timeline','/ui/compose':'compose','/ui/tasks/101':'board','/ui/doc/guide/console':'document'};
  if(names[pathname]) return route.fulfill({body:await fs.readFile('/tmp/hk-console-pages/'+names[pathname]+'.html','utf8'),contentType:'text/html'});
  return route.fulfill({status:404,body:'No fixture for '+pathname});
 });
 for(const [name,url] of Object.entries({inbox:'/ui/home',board:'/ui/queue',task:'/ui/tasks/101',decisions:'/ui/decisions',deploys:'/ui/deploys',fleet:'/ui/fleet',docs:'/ui/docs?key=guide%2Fconsole',grades:'/ui/grades',timeline:'/ui/timeline',compose:'/ui/compose',document:'/ui/doc/guide/console',palette:'/ui/queue'})) {
  if(selected.size && !selected.has(name))continue;
  errors.length=0; await page.goto('http://console.example'+url); await page.waitForTimeout(900);
  if(name==='board') {await page.getByText('Example builder model',{exact:true}).waitFor();await page.getByText('+1 sessions',{exact:true}).click();await page.getByText('Example reviewer model',{exact:true}).waitFor();await page.getByText('+1 sessions',{exact:true}).click();await page.locator('.hk-task-row').first().waitFor();await page.evaluate(()=>document.activeElement.blur());await page.keyboard.press('j');if(!await page.locator('.hk-task-row').first().evaluate(el=>el===document.activeElement))throw new Error('j did not activate first row');await page.locator('.hk-task-row').first().locator('a').first().click();await page.locator('#board-back').waitFor();await page.keyboard.press('Escape');await page.locator('.hk-filters').waitFor();await page.waitForFunction(()=>document.querySelector('.hk-task-row a')===document.activeElement);}
  if(name==='decisions') {
    await page.locator('input[type=radio]').first().check();
    await page.getByRole('button',{name:'이 항목만 답변'}).first().click();
    await page.getByRole('region',{name:'Confirm answer'}).waitFor(); if(writes)throw new Error('Answer submitted before confirmation');
    await page.getByRole('button',{name:'Confirm and submit'}).click();
    await page.getByRole('alert').filter({hasText:'Answer not confirmed'}).waitFor();
    if(!await page.locator('input[type=radio]').first().isChecked())throw new Error('Failed answer lost its selection');
  }
  if(name==='palette') {await page.getByRole('button',{name:/Search/}).click();await page.getByRole('dialog').waitFor();}
  const overflow=await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth);
  if(width===1280 && !await page.locator('.hk-side').evaluate(el=>el.getBoundingClientRect().bottom>=innerHeight))throw new Error('Sidebar does not reach viewport bottom');
  const file=`${name}-${theme}-${width}.png`; await page.screenshot({path:path.join(shots,file),fullPage:name!=='palette'});
  manifest.push({file,url,theme,width,overflow,errors:[...errors],captured_at:new Date().toISOString()});
 }
 await context.close();
 }
 const previous=selected.size ? JSON.parse(await fs.readFile(path.join(shots,'capture.json'),'utf8')).views : [];
 const merged=new Map(previous.map(item=>[item.file,item]));for(const item of manifest)merged.set(item.file,item);
 await fs.writeFile(path.join(shots,'capture.json'),JSON.stringify({browser:browser.version(),fixtures:'Fictional data; embedded production templates and committed bundles',views:[...merged.values()]},null,2)+'\n');
 const failures=manifest.filter(x=>x.overflow||x.errors.length); console.log(JSON.stringify(failures,null,2)); if(failures.length)process.exitCode=1;
 }finally{await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
