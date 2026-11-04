const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, '../internal/bridge/ui/index.html'),'utf8');
function page(fail = false, flush = true) {
  const start = html.indexOf('    function officialRoutingHTML(');
  const end = html.indexOf('    function renderPinModelRows(', start);
  assert.ok(start >= 0 && end > start, 'missing official routing UI');
  const state = {models:[{id:'ds',upstream_id:'cline-pass/deepseek-v4.1-flash-1017'}],modelBusy:false};
  const calls = [], messages = [], phases = [], renders = [];
  const context = vm.createContext({state, reasoningMappingBusy:false,
    esc:String, flushPinModels:async()=>flush,
    pinSaveState:(_id,msg,phase)=>{messages.push(msg);phases.push(phase);},
    renderPinModelRows(){},
    renderModels(data){renders.push(data);state.models=data.models;},
    api:async(url,options)=>{calls.push({url,...options}); if(fail) throw Error('network failed');return {models:[{...state.models[0], official_routing:options.body.enabled}]};}
  });
  vm.runInContext(html.slice(start,end)+';globalThis.toggle=toggleOfficialRouting;globalThis.markup=officialRoutingHTML;',context);
  return {state,calls,messages,phases,renders,context};
}
test('official routing toggle saves the selected model and can be disabled',async()=>{
 const p=page(); await p.context.toggle(p.state.models[0]);
 assert.equal(p.calls[0].url,'/models/official-routing');
 assert.deepEqual(JSON.parse(JSON.stringify(p.calls[0].body)),{model:'ds',upstream_id:'cline-pass/deepseek-v4.1-flash-1017',enabled:true});
 assert.equal(p.state.models[0].official_routing,true);
 await p.context.toggle(p.state.models[0]);
 assert.equal(p.state.models[0].official_routing,false);
 assert.equal(p.state.modelBusy,false);
});
test('failed official routing save keeps previous state and reports failure',async()=>{
 const p=page(true); await p.context.toggle(p.state.models[0]);
 assert.equal(p.state.models[0].official_routing,undefined);
 assert.equal(p.renders.length,0);
 assert.match(p.messages.at(-1),/保存失败/);
 assert.equal(p.phases.at(-1),'idle','do not offer unrelated whitelist retry');
 assert.equal(p.state.modelBusy,false);
});
test('failed pending draft save prevents official routing update',async()=>{
 const p=page(false,false); await p.context.toggle(p.state.models[0]);
 assert.equal(p.calls.length,0);
});
test('official routing button reports persisted state and is absent for other models',()=>{
 const p=page();
 assert.match(p.context.markup(p.state.models[0],0),/aria-pressed="false"/);
 p.state.models[0].official_routing=true;
 assert.match(p.context.markup(p.state.models[0],0),/aria-pressed="true"/);
 assert.equal(p.context.markup({upstream_id:'cline-pass/glm'},1),'');
});

test('rendering during another model save cannot leave routing button permanently disabled',()=>{
 const p=page(); p.state.modelBusy=true;
 assert.doesNotMatch(p.context.markup(p.state.models[0],0),/ disabled/);
});
