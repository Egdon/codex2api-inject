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
  return {textContent:'',className:'',hidden:false,disabled:false,style:{},attributes:{},
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
