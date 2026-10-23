package webui

import "testing"

func TestProviderModelPickerKeyboardAndEffortApplicationScope(t *testing.T) {
	runDesktopStatsNode(t, `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
const js=source['chat.js'], markup=source['index.html'];
assert.match(markup,/<button[^>]*id="modelBtn"[^>]*aria-haspopup="menu"/);
assert.match(markup,/<div[^>]*id="modelMenu"[^>]*role="menu"/);
const document={activeElement:null,getElementById:id=>elements[id]};
const focusable=()=>({attrs:{},style:{},setAttribute(k,v){this.attrs[k]=v},focus(){document.activeElement=this}});
const items=[focusable(),focusable(),focusable()];
const elements={modelBtn:focusable(),modelMenu:{style:{display:'block'},querySelectorAll:()=>items},effortBtn:focusable(),effortName:{textContent:''}};
const c={document,Array,modelMenuOpen:true,effortState:{supported:true,effort:'high',applies:'next turn'},uiText:(en,zh)=>zh,paintEffortMenu(){},effortLabel:v=>v};
vm.createContext(c);
function extract(start,end){const i=js.indexOf(start),j=js.indexOf(end,i+start.length);assert(i>=0&&j>i);return js.slice(i,j)}
vm.runInContext(extract('function modelMenuKeydown(event)', 'async function switchModel('),c);
let prevented=0;const key=value=>c.modelMenuKeydown({key:value,preventDefault(){prevented++}});
items[0].focus();key('ArrowDown');assert.equal(document.activeElement,items[1]);
key('End');assert.equal(document.activeElement,items[2]);key('ArrowDown');assert.equal(document.activeElement,items[0]);
key('Escape');assert.equal(elements.modelMenu.style.display,'none');assert.equal(elements.modelBtn.attrs['aria-expanded'],'false');assert.equal(document.activeElement,elements.modelBtn);assert.equal(prevented,4);
const toggle=extract('async function toggleModelMenu()', 'function modelMenuKeydown(');
assert.match(toggle,/<button type="button" role="menuitemradio"/);
assert.match(toggle,/event.stopPropagation\(\);switchModel/);
vm.runInContext(extract('function refreshEffortLanguage()', 'function paintEffortMenu()'),c);
c.refreshEffortLanguage();assert.match(elements.effortBtn.title,/下一个任务/);assert.doesNotMatch(elements.effortBtn.title,/下一次模型请求/);
c.effortState.applies='next model request';c.refreshEffortLanguage();assert.match(elements.effortBtn.title,/下一次模型请求/);
delete c.effortState.applies;c.refreshEffortLanguage();assert.match(elements.effortBtn.title,/下一个任务/);
`, "chat.js", "index.html")
}
