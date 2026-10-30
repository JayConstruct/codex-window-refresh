// Browser-only synthetic traffic. Production pages never import these records.
const ACCOUNTS = ['codex-01', 'codex-02', 'codex-07', 'codex-03'];
const MODEL = 'gpt-5-codex';
const reasonOrder = {session_affinity:70,parent_affinity:20,project_preference:20,quota_load:8,failover:5,host_selected:2};

function dataForToday() {
  const day = new Date(); day.setHours(10,45,12,0);
  const stamp = seconds => new Date(day.getTime() - seconds * 1000).toISOString();
  const attempts = [], users = [];
  const definitions = [
    ['request-01','project-backend','后端服务','调用方 A','s-a91','',[[0,'session_affinity','failed',429],[1,'failover','completed',200]],'completed',0],
    ['request-02','project-backend','后端服务','调用方 A','s-b17','s-parent-01',[[0,'parent_affinity','completed',200]],'completed',6],
    ['request-03','project-review','代码审查','调用方 B','s-c03','',[[1,'session_affinity','completed',200]],'completed',20],
    ['request-04','project-experiment','实验项目','调用方 C','s-d22','',[[2,'project_preference','completed',200]],'completed',42],
    ['request-05','project-backend','后端服务','调用方 A','s-e19','',[],'not_sent',61],
    ['request-06','','','调用方 B','','',[[0,'host_selected','completed',200]],'completed',74],
    ['request-07','project-backend','后端服务','调用方 A','s-f02','',[[0,'session_affinity','unknown',null]],'unknown',112],
  ];
  function make(id,project,projectName,caller,session,parentSession,steps,status,seconds) {
    const detail = steps.map(([index,reason,result,http], order) => {
      const attempt = {id:id+'-attempt-'+(order+1),selection_id:id+'-selection-'+(order+1),execution_id:id+'-execution-'+(order+1),account_id:ACCOUNTS[index],model:MODEL,provider:'codex',reason,status:result,created_at:stamp(seconds-order),updated_at:stamp(seconds-order-1),http_status:http,latency_ms:result==='unknown'?null:order?2900:600,ttft_ms:result==='completed'?280:null,input_tokens:result==='completed'?8120:null,output_tokens:result==='completed'?1420:null,cached_tokens:result==='completed'?6000:null,executed:true,caller,session,parent_session:parentSession,project,evidence:{priority:0,inflight:0,max_inflight:2,candidate_count:3,quota_fresh:true,snapshot_fresh:true,project_preferred:reason==='project_preference'||reason==='failover'}};
      attempts.push(attempt); return attempt;
    });
    const last=detail.at(-1), reported=detail.filter(a=>a.input_tokens!==null);
    const record={id,request_id:'req-'+id,project,project_name:projectName,caller,session,parent_session:parentSession,model:MODEL,account_id:last?.account_id||'',reason:last?.reason||'scheduler_busy',status,created_at:stamp(seconds),updated_at:stamp(seconds-4),attempt_count:detail.length,recovered:status==='completed'&&detail.some(a=>a.status==='failed'&&a.account_id!==last?.account_id),latency_ms:status==='completed'?(detail.length>1?3600:1420):null,ttft_ms:last?.ttft_ms??null,input_tokens:reported.length?reported.reduce((n,a)=>n+a.input_tokens,0):null,output_tokens:reported.length?reported.reduce((n,a)=>n+a.output_tokens,0):null,cached_tokens:reported.length?reported.reduce((n,a)=>n+a.cached_tokens,0):null,reported_usage_attempts:reported.length,attempted:detail.length>0,attempts:detail};
    users.push(record);return record;
  }
  for(const definition of definitions)make(...definition);
  const groups=[
    {project:'project-backend',name:'后端服务',caller:'调用方 A',requests:58,counts:[38,18,5,0],recoveries:3},
    {project:'project-review',name:'代码审查',caller:'调用方 B',requests:40,counts:[10,22,4,5],recoveries:1},
    {project:'project-experiment',name:'实验项目',caller:'调用方 C',requests:20,counts:[5,0,11,5],recoveries:1},
    {project:'',name:'',caller:'调用方 B',requests:2,counts:[2,0,0,0],recoveries:0},
  ];
  let serial=8;
  for(const group of groups){
    const existing=users.filter(r=>r.project===group.project),remaining=group.counts.map((n,index)=>n-existing.flatMap(r=>r.attempts).filter(a=>a.account_id===ACCOUNTS[index]).length);
    const create=(steps)=>make('request-'+String(serial++).padStart(3,'0'),group.project,group.name,group.caller,'s-'+group.project+'-'+serial,'',steps,'completed',180+serial*4);
    const recoveryCount=group.recoveries-existing.filter(r=>r.recovered).length;
    for(let i=0;i<recoveryCount;i++){const target=group.project==='project-experiment'?2:1;remaining[0]--;remaining[target]--;create([[0,'session_affinity','failed',429],[target,'failover','completed',200]]);}
    if(group.project==='project-backend'){remaining[0]-=2;create([[0,'session_affinity','failed',503],[0,'session_affinity','completed',200]]);}
    for(let index=0;index<remaining.length;index++)for(let i=0;i<remaining[index];i++)create([[index,'','completed',200]]);
  }
  const remainingReasons={...reasonOrder};for(const attempt of attempts)if(attempt.reason)remainingReasons[attempt.reason]--;
  const pool=Object.entries(remainingReasons).flatMap(([reason,n])=>Array(n).fill(reason));
  for(const attempt of attempts)if(!attempt.reason)attempt.reason=pool.shift();
  for(const record of users)if(record.attempts.length)record.reason=record.attempts.at(-1).reason;
  const plugin=[];
  function pluginRecord(id,kind,index,trigger,status,seconds,http=null,response=null) {
    const record={id,request_id:id,kind,trigger,account_id:ACCOUNTS[index],model:['wake','generation'].includes(kind)?MODEL:'',status,stage:kind==='query'?'quota_query':kind==='reset'?'official_cli':'completed',http_status:http,task_id:kind==='query'?'fixture-bulk':kind==='wake'?'persisted-wake-1':id,task_ids:[],created_at:stamp(seconds),updated_at:stamp(seconds-3),attempt_count:1,attempted:true,reason:'pinned',project:'',project_name:'',caller:'',session:'',parent_session:'',recovered:false,latency_ms:null,ttft_ms:null,input_tokens:null,output_tokens:null,cached_tokens:null,reported_usage_attempts:0,evidence:{delivery:http?'response_observed':kind==='reset'?'official_outcome_observed':'model_completed',policy_revision:17,model_revision:4},...(response?{response}:{})};
    if(kind==='reset')record.evidence.outcome='reset';plugin.push(record);return record;
  }
  pluginRecord('plugin-wake-01','wake',0,'five_hour_reset','verifying',0,200,{recorded_at:stamp(-3),stage:'completed',http_status:200,event:'response.completed',status:'completed',completed:true,response_text:'幂等表示重复执行同一操作，结果保持一致。',bytes:70,truncated:false,close_failed:false,error_code:''});
  pluginRecord('plugin-query-01','query',0,'wake_preflight','completed',9,200);
  pluginRecord('plugin-generation-01','generation',1,'manual_generation','completed',36,200,{stage:'completed',http_status:200,event:'response.completed',completed:true,response_text:'已生成 5 条短句候选。',bytes:40,truncated:false});
  pluginRecord('plugin-query-02','query',3,'periodic','failed',112,403);
  pluginRecord('plugin-reset-01','reset',0,'expiry_reset','completed',152);
  pluginRecord('plugin-wake-02','wake',2,'manual','unknown',237,200,{stage:'interrupted',http_status:200,event:'response.in_progress',completed:false,error_code:'wake_outcome_unknown',error_message:'响应读取中断，结果待核实。',response_text:'已保留部分响应摘要。',bytes:50,truncated:false});
  for(let i=0;i<9;i++)pluginRecord('plugin-query-'+(i+3),'query',i%4,'periodic','completed',300+i*20,200);
  for(let i=0;i<3;i++)pluginRecord('plugin-model-'+i,i%2?'generation':'wake',i%4,i%2?'manual_generation':'daily','completed',600+i*30,200,{stage:'completed',http_status:200,event:'response.completed',completed:true,bytes:0,truncated:false});
  return {users,plugin,stamp};
}

function matches(record, query) {
  for(const [key,field] of [['project','project'],['caller','caller'],['model','model'],['status','status'],['session','session']]){const value=query.get(key);if(value&&value!=='all'&&record[field]!==value)return false;}
  const account=query.get('account');if(account&&account!=='all'&&record.account_id!==account&&!record.attempts?.some(a=>a.account_id===account&&a.executed))return false;
  if(query.get('from')&&record.created_at<query.get('from'))return false;
  if(query.get('to')&&record.created_at>=query.get('to'))return false;
  const search=query.get('search')?.trim().toLowerCase();
  return !search||[record.id,record.request_id,record.project,record.project_name,record.caller,record.session,record.model,record.account_id,record.reason,record.kind,record.trigger,...(record.attempts||[]).flatMap(a=>[a.account_id,a.reason])].filter(Boolean).join(' ').toLowerCase().includes(search);
}

function aggregate(records) {
  const summary={requests:records.length,attempts:0,recovered:0,unknown:0},accounts=new Map(),reasons=new Map(),projects=new Map(),callers=new Map(),sessions=new Map();
  for(const record of records){
    summary.attempts+=record.attempt_count;summary.recovered+=Number(record.recovered);summary.unknown+=Number(record.status==='unknown');
    const rows=[];
    for(const [map,name,id]of [[projects,'project',record.project],[callers,'caller',record.caller],[sessions,'session',record.session]]){let row=map.get(id);if(!row){row={[name]:id,...name==='project'?{project_name:record.project_name}:{},requests:0,attempts:0,recovered:0,accounts:{}};map.set(id,row);}row.requests++;row.attempts+=record.attempt_count;row.recovered+=Number(record.recovered);rows.push(row);}
    for(const attempt of record.attempts||[]){if(!attempt.executed)continue;let account=accounts.get(attempt.account_id);if(!account){account={account_id:attempt.account_id,attempts:0,input_tokens:null,output_tokens:null,cached_tokens:null,reported_usage_attempts:0};accounts.set(attempt.account_id,account);}account.attempts++;for(const field of ['input_tokens','output_tokens','cached_tokens'])if(attempt[field]!==null&&attempt[field]!==undefined)account[field]=(account[field]??0)+attempt[field];if(attempt.input_tokens!==null||attempt.output_tokens!==null)account.reported_usage_attempts++;reasons.set(attempt.reason,(reasons.get(attempt.reason)||0)+1);for(const row of rows)row.accounts[attempt.account_id]=(row.accounts[attempt.account_id]||0)+1;}
  }
  return {summary,accounts:[...accounts.values()].sort((a,b)=>b.attempts-a.attempts),reasons:[...reasons].map(([reason,attempts])=>({reason,attempts})).sort((a,b)=>b.attempts-a.attempts),projects:[...projects.values()].sort((a,b)=>b.requests-a.requests),callers:[...callers.values()].sort((a,b)=>b.requests-a.requests),sessions:[...sessions.values()].sort((a,b)=>b.requests-a.requests)};
}

/** Return the exact traffic API payload for a local browser fixture URL. */
export function trafficFixture(url, identity) {
  const query=(url instanceof URL?url:new URL(url,'http://fixture.test')).searchParams,kind=query.get('kind')||'users';
  const fixture=dataForToday(),all=kind==='plugin'?fixture.plugin:fixture.users;
  const base={...(typeof identity==='function'?identity():identity),coverage:{status:'observing',reason:'initial_observation',last_seen:new Date().toISOString(),connected:true,baseline_complete:false,unlinked_attempts:0,request_limit:50000,attempt_limit:100000},records:[],page:1,limit:7,total:0,accounts:[],reasons:[],projects:[],callers:[],sessions:[],summary:{requests:0,attempts:0,recovered:0,unknown:0},facets:{projects:[],callers:[],accounts:[],models:[]}};
  const id=query.get('id')||query.get('request_id');
  if(id){const record=all.find(record=>record.id===id||record.request_id===id);if(!record)return {...base,error:'record_not_found'};return {...base,record:structuredClone(record),records:[structuredClone(record)],total:1};}
  const records=all.filter(record=>matches(record,query));Object.assign(base,aggregate(records));
  base.facets={projects:[...new Map(records.filter(r=>r.project).map(r=>[r.project,{id:r.project,name:r.project_name}])).values()],callers:[...new Set(records.map(r=>r.caller).filter(Boolean))],accounts:[...new Set(records.flatMap(r=>[r.account_id,...(r.attempts||[]).map(a=>a.account_id)]).filter(Boolean))],models:[...new Set(records.map(r=>r.model).filter(Boolean))]};
  base.total=records.length;const limit=Math.min(100,Math.max(1,Number(query.get('limit')||(kind==='plugin'?6:7)))),pages=Math.max(1,Math.ceil(records.length/limit));base.limit=limit;base.page=Math.min(pages,Math.max(1,Number(query.get('page')||1)));base.records=records.slice((base.page-1)*limit,base.page*limit).map(record=>{const copy=structuredClone(record);delete copy.attempts;return copy;});
  return base;
}
