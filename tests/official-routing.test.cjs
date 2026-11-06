const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(require('node:path').join(__dirname,'../internal/bridge/ui/index.html'),'utf8');
const start=html.indexOf('    function updateProviderPolicyHint(');
const end=html.indexOf('    async function flushProviderPolicy(',start);
function hint(policy){const nodes={providerPolicy:{value:policy},providerPolicyHint:{textContent:''}};const context=vm.createContext({$:id=>nodes[id]});vm.runInContext(html.slice(start,end)+';updateProviderPolicyHint();',context);return nodes.providerPolicyHint.textContent;}
test('unpinned policy explains inactive saved routing and explicit policies explain fallback',()=>{assert.match(hint('none'),/不发送/);assert.match(hint('none'),/配置保留/);assert.match(hint('config'),/移除后重试一次/);assert.match(hint('config'),/不能保证固定提供方/);});
test('client provider policy retains client parameter priority',()=>{assert.match(hint('client'),/优先使用客户端/);assert.match(hint('client'),/未设置/);});
