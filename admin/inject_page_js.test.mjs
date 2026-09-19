// Run manually: node --test admin/inject_page_js.test.mjs
// Offline regression checks only; no browser, network, or live account access.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('./inject_page.go', import.meta.url), 'utf8');
const script = source.split('<script>')[1].split('</script>')[0];
const definitions = script.slice(0, script.indexOf("if(window.matchMedia"));
function element() {
  return {value:'',dataset:{},textContent:'',className:'',hidden:false,disabled:false,style:{},attributes:{},
    classList:{toggle(){},add(){},remove(){}},
    replaceChildren(){},add(){},reportValidity(){return true;},
    setAttribute(key,value){this.attributes[key]=value;}};
}
function fixture() {
  const elements = new Map();
  const document = {documentElement:{dataset:{cfgLoaded:'1'}},querySelectorAll(){return [];},getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);}};
  const context = vm.createContext({document,Option:function(text,value){this.text=text;this.value=value;},localStorage:{getItem(){return null;}},sessionStorage:{getItem(){return null;},setItem(){}},performance:{now(){return 1000;}},Date,console,setTimeout,clearTimeout});
  vm.runInContext(definitions,context);
  return {context,elements,run:code=>vm.runInContext(code,context)};
}

test('visible select-all preserves hidden selected accounts',()=> {
  const f=fixture();
  f.run('data={accounts:[{id:1},{id:2},{id:3}]}; visibleAccountIDs=new Set([1,2]); selectedAccounts.add(3); toggleAllAccounts(true)');
  assert.equal(f.run('selectedAccounts.size'),3);
  assert.equal(f.elements.get('selectionCount').textContent,'已选择 3 个（隐藏 1 个）');
  f.run('toggleAllAccounts(false)');
  assert.equal(f.run('selectedAccounts.size'),1);
  assert.equal(f.run('selectedAccounts.has(3)'),true);
});

test('plan aliases stay distinct from Pro',()=> {
  const f=fixture();
  for(const alias of ['prolite',' PRO_LITE ','pro-lite']) {
    f.context.alias=alias;
    assert.equal(f.run('harvestPlanClass(alias)'),'prolite');
  }
  assert.equal(f.run("harvestPlanClass('Pro')"),'pro');
  assert.equal(f.run("harvestPlanClass('plus')"),'plus');
  assert.equal(f.run("harvestPlanClass('team')"),'other');
});

test('plan, state and search intersect without network or dirty configuration',()=> {
  const f=fixture();
  f.context.fetch=()=>{throw new Error('filter must not fetch');};
  f.run(`
    data={config:{models:['astra']},accounts:[
      {id:1,email:'alpha@example.com',plan_type:'pro',tickets:[]},
      {id:2,email:'alpha-lite@example.com',plan_type:'pro_lite',tickets:[]},
      {id:3,email:'other@example.com',plan_type:'pro-lite',tickets:[]}
    ]};
    $('accountSearch').value='alpha'; $('accountFilter').value='missing';
    $('accountPlanFilter').value='prolite'; selectedAccounts.add(1); applyFilters();
  `);
  assert.equal(f.run('JSON.stringify([...visibleAccountIDs])'),'[2]');
  assert.equal(f.run('cfgDirty'),false);
  f.run('toggleAllAccounts(true)');
  assert.equal(f.run('JSON.stringify([...selectedAccounts])'),'[1,2]');
  assert.equal(f.elements.get('selectionCount').textContent,'已选择 2 个（隐藏 1 个）');
  f.run("$('accountSearch').value='not-found'; applyFilters()");
  assert.equal(f.run('visibleAccountIDs.size'),0);
  assert.equal(f.elements.get('selectAll').disabled,true);
});

test('default weight controls mark changes as unsaved',()=> {
  const f=fixture(); f.run('resetPlanWeights()');
  assert.equal(f.elements.get('plan_weight_pro').value,3);
  assert.equal(f.elements.get('plan_weight_prolite').value,2);
  assert.equal(f.elements.get('plan_weight_plus').value,1);
  assert.equal(f.run('cfgDirty'),true);
});

test('branding rejects script URLs and broken images restore all icons',()=> {
  const f=fixture();
  assert.equal(f.run("sanitizeSiteLogo('javascript:alert(1)')"),'');
  assert.equal(f.run("sanitizeSiteLogo('//example.com/icon.png')"),'');
  assert.equal(f.run("sanitizeSiteLogo('/favicon.png')"),'/favicon.png');
  const images=[{},{}]; f.context.document.querySelectorAll=()=>images;
  f.run("applySiteLogo('https://example.com/logo.png')");
  assert.equal(f.elements.get('siteFavicon').href,'https://example.com/logo.png');
  images[0].onerror();
  assert.equal(f.elements.get('siteFavicon').href,'/favicon.png');
  assert.equal(f.elements.get('siteTouchIcon').href,'/favicon.png');
  assert.ok(images.every(img=>img.src==='/favicon.png' && img.onerror===null));
});

test('Astra threshold is an integer percentage control tracked for unsaved changes',()=> {
  assert.match(source, /id="astra_miss_threshold_percent" type="number" min="1" max="100" step="1" value="80" required/);
  const f=fixture();
  assert.equal(f.run("configInputs.includes('astra_miss_threshold_percent')"),true);
});

test('Astra threshold loads configured values and resets legacy config to 80',()=> {
  const f=fixture();
  f.run('fillCfg({astra_miss_threshold_percent:95})');
  assert.equal(f.elements.get('astra_miss_threshold_percent').value,95);
  f.run('fillCfg({})');
  assert.equal(f.elements.get('astra_miss_threshold_percent').value,80);
  f.run('fillCfg({astra_miss_threshold_percent:null})');
  assert.equal(f.elements.get('astra_miss_threshold_percent').value,80);
});

test('Astra threshold save validates before sending and uses a numeric payload',async()=> {
  const f=fixture();
  f.context.fetch=()=>{throw new Error('save fixture must not access network');};
  f.run(`
    data={config:{}}; fillCfg({models:['gpt-6-astra']});
    $('astra_miss_threshold_percent').value='85';
    mutate=work=>work();
    api=async(path,opts)=>{globalThis.savedPath=path;globalThis.savedBody=JSON.parse(opts.body);return globalThis.savedBody;};
  `);
  const input=f.elements.get('astra_miss_threshold_percent');
  input.reportValidity=()=>false;
  await f.run('saveCfg()');
  assert.equal(f.context.savedBody,undefined);
  input.reportValidity=()=>true;
  await f.run('saveCfg()');
  assert.equal(f.context.savedPath,'/settings/turn-state');
  assert.equal(f.context.savedBody.astra_miss_threshold_percent,85);
});

test('Astra dual-action controls retain explicit zero and share dirty protection',async()=> {
  const f=fixture(); f.run('fillCfg({astra_failure_priority:0})');
  assert.equal(f.elements.get('astra_failure_priority').value,0);
  assert.equal(f.elements.get('astra_group_failure_batches').value,2);
  assert.equal(f.elements.get('astra_priority_failure_batches').value,1);
  assert.equal(f.elements.get('astra_priority_policy_enabled').dataset.on,'0');
  for(const field of ['astra_group_failure_batches','astra_priority_failure_batches','astra_failure_priority','astra_recovery_priority']) {
    f.context.field=field;
    assert.equal(f.run('configInputs.includes(field)'),true);
  }
  f.run(`data={config:{}}; groupsAttempted=true; render=()=>{};
    api=()=>new Promise(resolve=>{globalThis.finishLoad=resolve;});`);
  const loading=f.run('load({fillCfg:true})');
  f.run(`$('astra_failure_priority').value='-7'; setSwitch('astra_priority_policy_enabled',true); markCfgDirty();
    finishLoad({config:{astra_failure_priority:-1,astra_priority_policy_enabled:false},accounts:[]});`);
  await loading;
  assert.equal(f.elements.get('astra_failure_priority').value,'-7');
  assert.equal(f.elements.get('astra_priority_policy_enabled').dataset.on,'1');
  assert.equal(f.run('cfgDirty'),true);
});

test('Astra priority-only save needs no groups and warns without blocking reverse priorities',async()=> {
  const f=fixture();
  f.run(`data={config:{}}; fillCfg({models:['gpt-6-astra'],astra_priority_policy_enabled:true,astra_failure_priority:0,astra_recovery_priority:-1});
    mutate=work=>work(); api=async(path,opts)=>{globalThis.savedBody=JSON.parse(opts.body);return savedBody;};`);
  assert.equal(f.elements.get('astraPriorityWarning').hidden,false);
  await f.run('saveCfg()');
  assert.equal(f.context.savedBody.astra_priority_policy_enabled,true);
  assert.equal(f.context.savedBody.astra_policy_enabled,false);
  assert.equal(f.context.savedBody.astra_failure_priority,0);
  assert.equal(f.context.savedBody.astra_recovery_priority,-1);
  assert.equal(f.context.savedBody.astra_group_failure_batches,2);
  assert.equal(f.context.savedBody.astra_priority_failure_batches,1);
});

test('Astra dual-action invalid ranges reject save even when native validity is unavailable',async()=> {
  for(const [field,value] of [['astra_group_failure_batches','0'],['astra_group_failure_batches','51'],['astra_priority_failure_batches','1.5'],['astra_failure_priority','101'],['astra_recovery_priority','-101'],['astra_failure_priority','']]) {
    const f=fixture(); f.context.field=field; f.context.value=value;
    f.run(`data={config:{}}; fillCfg({}); $(field).value=value; mutate=()=>{throw new Error('invalid config submitted');};`);
    await f.run('saveCfg()');
    assert.match(f.elements.get('actionMsg').textContent,/必须为/);
  }
});

test('Astra cards show independent fired owned suppressed states and configured targets',()=> {
  const f=policyFixture({enabled:true,consecutive_failures:4,group_triggered:true,group_suppressed:true,priority_demoted:true,priority_triggered:true,failure_priority:-3});
  f.run(`data={config:{astra_policy_enabled:true,astra_priority_policy_enabled:true,astra_group_failure_batches:3,astra_priority_failure_batches:5,astra_failure_group_id:11,astra_recovery_group_id:12,astra_failure_priority:0,astra_recovery_priority:7}}; renderPolicy(view)`);
  const shown=f.context.view.policy.textContent;
  assert.match(shown,/共享连续失败 4（迁组阈值 3，优先级阈值 5）/);
  assert.match(shown,/迁组开启：已触发 \/ 未持有 \/ 人工覆盖后抑制；目标 #11 → #12/);
  assert.match(shown,/优先级开启：已触发 \/ 策略持有 \/ 未抑制；目标 0 → 7/);
  assert.match(shown,/策略持有的优先级 -3/);
  assert.doesNotMatch(shown,/连续失败 4\/2/);
  assert.equal(policyFixture({priority_suppressed:true}).context.view.policy.hidden,false);
  const manual=policyFixture({error:'manual_priority_changed'}).context.view.policy.textContent;
  assert.match(manual,/人工优先级已变更.*不影响共享失败计数或迁组动作/);
});

function policyFixture(policy) {
  const f=fixture();
  f.context.view={account:{astra_policy:policy},policy:element()};
  f.run('renderPolicy(view)');
  return f;
}

test('Astra latest batch shows frozen counters and qualification, not an inferred streak',()=> {
  const f=policyFixture({enabled:true,consecutive_failures:1,last_outcome:'ordinary_miss',latest_batch:{attempts:20,max_attempts:20,ordinary_misses:16,threshold_percent:80,exhausted:true,reason:'qualified_miss'}});
  f.run('data={config:{astra_miss_threshold_percent:95}}; renderPolicy(view)');
  const shown=f.context.view.policy.textContent;
  assert.match(shown,/共享连续失败 1（迁组阈值 2，优先级阈值 1）/);
  assert.match(shown,/尝试 20\/20，普通未命中 16，批次阈值 80%，已耗尽/);
  assert.match(shown,/批次判定：符合普通未命中失败条件/);
  assert.match(shown,/符合条件不代表已计数/);
  assert.doesNotMatch(shown,/95%|已计入连续失败/);
  assert.equal(f.context.view.account.astra_policy.consecutive_failures,1);
});

test('Astra evidence-only state is visible and all nonqualifying reasons are explained',()=> {
  for(const [reason,label] of Object.entries({insufficient_misses:'普通未命中占比不足',success:'获得292',confirmation_warning:'292 确认警告',interrupted:'异常或中断',not_exhausted:'未耗尽全部尝试'})) {
    const f=policyFixture({latest_batch:{attempts:0,max_attempts:20,ordinary_misses:0,threshold_percent:80,exhausted:false,reason}});
    const shown=f.context.view.policy.textContent;
    assert.equal(f.context.view.policy.hidden,false);
    assert.ok(shown.includes('批次判定：'+label+'，不计失败'));
    assert.match(shown,/尝试 0\/20，普通未命中 0/);
    assert.doesNotMatch(shown,/符合普通未命中失败条件/);
  }
});

test('Astra guard errors take precedence over outcomes without hiding evidence',()=> {
  for(const error of ['manual_membership_changed','policy_state_unavailable','other_error']) {
    const f=policyFixture({enabled:true,error,consecutive_failures:0,last_outcome:'ordinary_miss',latest_batch:{attempts:20,max_attempts:20,ordinary_misses:20,threshold_percent:80,exhausted:true,reason:'qualified_miss'}});
    const shown=f.context.view.policy.textContent;
    assert.match(shown,/共享连续失败 0（迁组阈值 2，优先级阈值 1）/);
    assert.match(shown,/普通未命中 20/);
    assert.doesNotMatch(shown,/本批符合普通未命中失败条件|已计入连续失败/);
    if(error==='manual_membership_changed') {
      assert.match(shown,/人工分组已变更.*不影响共享失败计数或优先级动作/);
      assert.ok(shown.indexOf('人工分组已变更')<shown.indexOf('批次判定'));
    }
  }
  const f=policyFixture({error:'manual_membership_changed',last_outcome:'recovered'});
  assert.doesNotMatch(f.context.view.policy.textContent,/已自动恢复/);
});

test('Astra legacy outcomes have unavailable counts, never fabricated batch evidence',()=> {
  for(const latest_batch of [undefined,null]) {
    const f=policyFixture({last_outcome:'ordinary_miss',consecutive_failures:1,latest_batch});
    assert.match(f.context.view.policy.textContent,/最近一批计数不可用（旧版状态）/);
    assert.match(f.context.view.policy.textContent,/共享连续失败 1（迁组阈值 2，优先级阈值 1）/);
    assert.match(f.context.view.policy.textContent,/是否符合当前阈值未知/);
    assert.doesNotMatch(f.context.view.policy.textContent,/尝试 \d|批次阈值|已计入|本批符合普通未命中失败条件/);
  }
  assert.equal(policyFixture(undefined).context.view.policy.hidden,true);
  assert.equal(policyFixture({}).context.view.policy.hidden,true);
});

test('proxy selector exposes independent controls with safe legacy defaults',()=> {
  const f=fixture(); f.run('fillCfg({})');
  assert.equal(f.elements.get('harvest_proxy_provider').value,'zooproxy');
  assert.equal(f.elements.get('zooproxy_panel').hidden,false);
  assert.equal(f.elements.get('litport_panel').hidden,true);
  assert.equal(f.elements.get('litport_host').value,'hub-us-10.litport.net:1337');
  assert.equal(f.elements.get('litport_username').value,'');
  assert.equal(f.elements.get('litport_region').value,'DE');
  assert.equal(f.elements.get('litport_region_mode').value,'fixed');
  assert.equal(f.elements.get('litport_session_seconds').value,600);
  assert.match(source,/id="litport_session_seconds" type="number" min="1" max="86400" step="1" value="600" required/);
  for(const field of ['harvest_proxy_provider','litport_host','litport_username','litport_password','litport_region','litport_region_mode','litport_session_seconds']) {
    f.context.field=field;
    assert.equal(f.run('configInputs.includes(field)'),true);
  }
  assert.doesNotMatch(source,/ZooProxy 出口/);
});

test('provider switching changes visibility only, preserving hidden edits and passwords',()=> {
  const f=fixture();
  f.run(`fillCfg({}); $('zoo_user_prefix').value='zoo-fixture'; $('zoo_password').value='zoo-test-secret';
    $('litport_username').value='litport-fixture'; $('litport_password').value='litport-test-secret';
    $('harvest_proxy_provider').value='litport'; updateProxyPanels(); markCfgDirty();`);
  assert.equal(f.elements.get('zooproxy_panel').hidden,true);
  assert.equal(f.elements.get('litport_panel').hidden,false);
  f.run("$('harvest_proxy_provider').value='zooproxy'; updateProxyPanels(); markCfgDirty()");
  assert.equal(f.elements.get('zoo_user_prefix').value,'zoo-fixture');
  assert.equal(f.elements.get('zoo_password').value,'zoo-test-secret');
  assert.equal(f.elements.get('litport_username').value,'litport-fixture');
  assert.equal(f.elements.get('litport_password').value,'litport-test-secret');
  assert.equal(f.run('cfgDirty'),true);
  assert.equal(f.run('cfgEditRevision'),2);
});

test('fill never echoes either password, using only saved flags',()=> {
  const f=fixture();
  f.run(`fillCfg({zoo_password:'not-for-display',litport_password:'not-for-display',zoo_password_set:true,litport_password_set:true})`);
  for(const id of ['zoo_password','litport_password']) {
    assert.equal(f.elements.get(id).value,'');
    assert.equal(f.elements.get(id).placeholder,'已保存，留空不改');
  }
});

test('shared save sends both configurations and clears both passwords only on matching revision',async()=> {
  const f=fixture();
  f.run(`data={config:{}}; fillCfg({models:['gpt-6-astra']});
    $('zoo_host').value='zoo.example:5000'; $('zoo_user_prefix').value='zoo-fixture';
    $('zoo_password').value='zoo-test-secret'; $('litport_password').value='litport-test-secret';
    $('litport_username').value='litport-fixture'; $('litport_region').value='FR';
    $('litport_region_mode').value='rotation'; $('litport_session_seconds').value='900';
    $('harvest_proxy_provider').value='litport'; updateProxyPanels(); markCfgDirty();
    mutate=work=>work(); api=async(path,opts)=>{globalThis.savedBody=JSON.parse(opts.body);return {...savedBody,zoo_password_set:true,litport_password_set:true};};`);
  await f.run('saveCfg()');
  assert.equal(f.context.savedBody.harvest_proxy_provider,'litport');
  assert.equal(f.context.savedBody.zoo_host,'zoo.example:5000');
  assert.equal(f.context.savedBody.zoo_user_prefix,'zoo-fixture');
  assert.equal(f.context.savedBody.litport_username,'litport-fixture');
  assert.equal(f.context.savedBody.litport_region,'FR');
  assert.equal(f.context.savedBody.litport_region_mode,'rotation');
  assert.equal(f.context.savedBody.litport_session_seconds,900);
  assert.equal(f.context.savedBody.zoo_password,'zoo-test-secret');
  assert.equal(f.context.savedBody.litport_password,'litport-test-secret');
  assert.equal(f.elements.get('zoo_password').value,'');
  assert.equal(f.elements.get('litport_password').value,'');
  assert.equal(f.run('cfgDirty'),false);

  f.run(`api=(path,opts)=>{globalThis.savedBody=JSON.parse(opts.body);return new Promise(resolve=>{globalThis.finishSave=resolve;});}; markCfgDirty();`);
  const saving=f.run('saveCfg()');
  f.run(`$('litport_password').value='later-litport-secret'; $('zoo_password').value='later-zoo-secret';
    $('harvest_proxy_provider').value='zooproxy'; updateProxyPanels(); markCfgDirty(); finishSave(savedBody);`);
  await saving;
  assert.equal(f.elements.get('litport_password').value,'later-litport-secret');
  assert.equal(f.elements.get('zoo_password').value,'later-zoo-secret');
  assert.equal(f.elements.get('harvest_proxy_provider').value,'zooproxy');
  assert.equal(f.run('cfgDirty'),true);
});

test('poll and manual refresh preserve dirty provider and hidden fields, even when edits arrive in flight',async()=> {
  const f=fixture();
  f.run(`data={config:{}}; fillCfg({}); groupsAttempted=true; render=()=>{};
    api=()=>new Promise(resolve=>{globalThis.finishLoad=resolve;});`);
  const loading=f.run('load({fillCfg:true})');
  f.run(`$('harvest_proxy_provider').value='litport'; updateProxyPanels();
    $('litport_password').value='typed-litport-secret'; $('zoo_password').value='typed-zoo-secret';
    $('zoo_host').value='edited.example:5000'; markCfgDirty();
    finishLoad({config:{harvest_proxy_provider:'zooproxy',zoo_host:'stale.example:5000'},accounts:[]});`);
  await loading;
  assert.equal(f.elements.get('harvest_proxy_provider').value,'litport');
  assert.equal(f.elements.get('zoo_host').value,'edited.example:5000');
  assert.equal(f.elements.get('litport_password').value,'typed-litport-secret');
  assert.equal(f.elements.get('zoo_password').value,'typed-zoo-secret');
  f.run('api=async()=>({config:{},accounts:[]})');
  await f.run('load({fillCfg:true})');
  assert.equal(f.elements.get('harvest_proxy_provider').value,'litport');
  assert.equal(f.elements.get('zoo_host').value,'edited.example:5000');
  assert.equal(f.run('cfgDirty'),true);
});

test('invalid Litport TTL and unknown provider never submit a save',async()=> {
  const f=fixture();
  f.run(`data={config:{}}; fillCfg({}); mutate=()=>{throw new Error('must not submit');};`);
  f.elements.get('litport_session_seconds').reportValidity=()=>false;
  await f.run('saveCfg()');
  assert.match(f.elements.get('actionMsg').textContent,/1–86400/);
  f.elements.get('litport_session_seconds').reportValidity=()=>true;
  f.run("$('harvest_proxy_provider').value='unknown'");
  await f.run('saveCfg()');
  assert.match(f.elements.get('actionMsg').textContent,/有效的采集代理/);
});

test('phase uses actual attempt provider, not selected config, without diagnostic secrets',()=> {
  const f=fixture();
  f.context.view={phase:element(),kind:element(),confirm:element(),copy:element(),error:element()};
  f.run(`data={config:{harvest_proxy_provider:'zooproxy'},job:{status:'running'}};
    updateTicket(view,{}, {phase:'confirming',provider:'litport',region:'FR',attempt:2,max:20,
      status:'http://fixture:secret@example.test/session-123',detail:'session-123 fixture secret'});`);
  assert.equal(f.context.view.phase.textContent,'确认 · 尝试 2/20 · Litport · 请求地区 FR');
  assert.equal(f.context.view.phase.title,f.context.view.phase.textContent);
  assert.doesNotMatch(f.context.view.phase.title,/http|secret|session/);
  f.run(`updateTicket(view,{}, {phase:'running',provider:'zooproxy',region:'DE',attempt:3,max:20})`);
  assert.match(f.context.view.phase.textContent,/ZooProxy · 请求地区 DE/);
  f.run(`updateTicket(view,{}, {phase:'queued',provider:'http://secret',region:'session-secret',attempt:0,max:20})`);
  assert.equal(f.context.view.phase.textContent,'排队 · 尝试 0/20');
});

test('remaining time uses server anchor, not stale remaining_sec',()=> {
  const f=fixture();
  assert.equal(f.run('clockAnchor={unix:2000,mono:0}; remaining({issued_unix:1900,remaining_sec:9})'),3499);
});
test('ticket countdown survives a completed job cell',()=> {
  const f=fixture();
  f.context.view={phase:element(),kind:element(),confirm:element(),copy:element(),error:element(),countdown:element(),bar:element(),fill:element(),dot:element(),lifecycle:element()};
  f.run("data={job:{status:'done'}}; updateTicket(view,{length:292,token:'fixture',issued_unix:1000},{phase:'succeeded',status:'292',attempt:1,max:20}); updateClock(view,1100)");
  assert.equal(f.context.view.phase.hidden,true);
  assert.equal(f.context.view.countdown.textContent,'58:20 剩余');
});
test('active probing is separate from ticket time',()=> {
  const f=fixture();
  f.context.view={phase:element(),kind:element(),confirm:element(),copy:element(),error:element(),countdown:element(),bar:element(),fill:element(),dot:element(),lifecycle:element()};
  f.run("data={job:{status:'running'}}; updateTicket(view,{length:292,token:'fixture',issued_unix:1000},{phase:'confirming',status:'确认',attempt:2,max:20}); updateClock(view,1100)");
  assert.equal(f.context.view.phase.hidden,false);
  assert.equal(f.context.view.phase.textContent,'确认 · 尝试 2/20');
  assert.equal(f.context.view.countdown.textContent,'58:20 剩余');
});
test('terminal task has its own progress and a sticky dismissed summary',()=> {
  const f=fixture();
  f.run("setProgress('poolProgress',80,120); data={job:{id:'job1',status:'done',done:1,total:1,succeeded:1,finished_unix:1000}}; renderJob()");
  assert.equal(f.elements.get('poolProgressText').textContent,'80/120 · 67%');
  assert.equal(f.elements.get('taskProgressText').textContent,'1/1 · 100%');
  assert.equal(f.elements.get('cancelJobBtn').disabled,true);
  f.run('dismissJob(); renderJob()');
  assert.equal(f.elements.get('jobPanel').hidden,true);
});
