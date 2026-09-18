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
    setAttribute(key,value){this.attributes[key]=value;}};
}
function fixture() {
  const elements = new Map();
  const document = {querySelectorAll(){return [];},getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id);}};
  const context = vm.createContext({document,localStorage:{getItem(){return null;}},sessionStorage:{getItem(){return null;},setItem(){}},performance:{now(){return 1000;}},Date,console,setTimeout,clearTimeout});
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
