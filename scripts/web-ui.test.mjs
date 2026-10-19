import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

const source=readFileSync(new URL('../internal/web/ui/app.js',import.meta.url),'utf8');
const start=source.indexOf('let updateInfo = null;');
const end=source.indexOf('// notes renders',start);
assert(start>=0&&end>start,'Release notification code exists');
const available={available:true,latest:{version:'v0.5.0'},state:'idle'};
function harness(api, storage = new Map()){
  const localStorage={getItem:key=>storage.get(key)||null,setItem:(key,value)=>storage.set(key,value)};
  const notice={hidden:true,children:[],replaceChildren(...nodes){this.children=nodes;}};
  let clock=1_000_000;
  const context=vm.createContext({localStorage,document:{hidden:false},Date:{now:()=>clock},api,
    $:()=>notice,renderFoot:()=>{},el:(tag,attrs,...children)=>({tag,attrs,children})});
  vm.runInContext(source.slice(start,end),context);
  return {context,notice,advance:ms=>{clock+=ms;},check:()=>vm.runInContext('checkForUpdates()',context)};
}
test('Checks from any page, announces a newer version, and only requests status',async()=>{
  const paths=[];const h=harness(async path=>{paths.push(path);return available;});
  await h.check();assert.deepEqual(paths,['/api/update?check=1']);assert.equal(h.notice.hidden,false);
  const link=h.notice.children[1];assert.equal(link.attrs.href,'#/settings/updates');
  assert.equal(link.tag,'a');assert.match(JSON.stringify(h.notice.children),/v0.5.0/);
  await h.check();assert.equal(paths.length,1,'Reopening/visibility events are throttled');
  h.advance(5*60*1000);await h.check();assert.equal(paths.length,2);
});
test('Hidden tabs skip checks and simultaneous requests share one in-flight check',async()=>{
  let calls=0,finish;const h=harness(()=>{calls++;return new Promise(resolve=>{finish=resolve;});});
  h.context.document.hidden=true;await h.check();assert.equal(calls,0);
  h.context.document.hidden=false;const pending=h.check();await h.check();assert.equal(calls,1);
  finish(available);await pending;assert.equal(h.notice.hidden,false);
});
test('Offline failures recover; current versions hide the update notice',async()=>{
  let fail=true;const h=harness(async()=>{if(fail)throw Error('offline');return {available:false,state:'idle'};});
  await h.check();assert.equal(h.notice.hidden,true);
  fail=false;h.advance(5*60*1000);await h.check();assert.equal(h.notice.hidden,true);
  vm.runInContext('updateInfo = '+JSON.stringify(available)+';renderUpdateNotice()',h.context);assert.equal(h.notice.hidden,false);
  vm.runInContext('updateInfo = {available:false,state:"idle"};renderUpdateNotice()',h.context);assert.equal(h.notice.hidden,true);
});

test('Dismissing a release survives checks and reloads, without hiding the next release',async()=>{
  const storage=new Map();const h=harness(async()=>available,storage);
  await h.check();assert.equal(h.notice.hidden,false);
  const dismiss=h.notice.children.find(node=>node.attrs['aria-label']==='Dismiss update notification');
  assert(dismiss,'Accessible dismiss action');dismiss.attrs.onclick();assert.equal(h.notice.hidden,true);
  h.advance(5*60*1000);await h.check();assert.equal(h.notice.hidden,true);
  const reopened=harness(async()=>available,storage);await reopened.check();assert.equal(reopened.notice.hidden,true);
  vm.runInContext('updateInfo = {available:true,latest:{version:"v0.6.0"},state:"idle"};renderUpdateNotice()',reopened.context);
  assert.equal(reopened.notice.hidden,false,'A different release is announced');
});
test('Disabled browser storage does not break checking or dismissal',async()=>{
  const storage={get(){throw Error('blocked');},set(){throw Error('blocked');}};
  const h=harness(async()=>available,storage);await h.check();assert.equal(h.notice.hidden,false);
  h.notice.children.find(node=>node.attrs['aria-label']==='Dismiss update notification').attrs.onclick();
  assert.equal(h.notice.hidden,true);
});
