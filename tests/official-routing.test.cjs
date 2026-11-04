const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(require('node:path').join(__dirname,'../internal/bridge/ui/index.html'),'utf8');
const start=html.indexOf('    function updateProviderPolicyHint(');
const end=html.indexOf('    async function flushProviderPolicy(',start);
function hint(policy){const nodes={providerPolicy:{value:policy},providerPolicyHint:{textContent:''}};const context=vm.createContext({$:id=>nodes[id]});vm.runInContext(html.slice(start,end)+';updateProviderPolicyHint();',context);return nodes.providerPolicyHint.textContent;}
test('default provider policy explains automatic DeepSeek region constraint',()=>{assert.match(hint('config'),/DeepSeek 1017/);assert.match(hint('config'),/deepseek/);assert.match(hint('config'),/区域约束/);});
test('client provider policy retains client parameter priority',()=>{assert.match(hint('client'),/优先使用客户端/);assert.match(hint('client'),/未设置/);});
