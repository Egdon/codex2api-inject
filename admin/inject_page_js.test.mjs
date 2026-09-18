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
  const document = {querySelectorAll(){return [];},getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);}};
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
  assert.match(shown,/连续失败 1\/2/);
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
    assert.match(shown,/连续失败 0\/2/);
    assert.match(shown,/普通未命中 20/);
    assert.doesNotMatch(shown,/本批符合普通未命中失败条件|已计入连续失败/);
    if(error==='manual_membership_changed') {
      assert.match(shown,/人工分组已变更.*本批未计入连续失败/);
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
    assert.match(f.context.view.policy.textContent,/连续失败 1\/2/);
    assert.match(f.context.view.policy.textContent,/是否符合当前阈值未知/);
    assert.doesNotMatch(f.context.view.policy.textContent,/尝试 \d|批次阈值|已计入|本批符合普通未命中失败条件/);
  }
  assert.equal(policyFixture(undefined).context.view.policy.hidden,true);
  assert.equal(policyFixture({}).context.view.policy.hidden,true);
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
