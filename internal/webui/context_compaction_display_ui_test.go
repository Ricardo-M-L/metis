package webui

import "testing"

func TestDesktopContextCompactionStagesKeepCommittedUsageVisible(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'))['app.js'];
function extract(start, end) {
  const from = source.indexOf(start), to = source.indexOf(end, from);
  assert.ok(from >= 0 && to > from, 'cannot isolate context meter');
  return source.slice(from, to);
}
function element() {
  const attrs = {}, classes = new Set();
  return {attrs, dataset:{}, hidden:true, innerHTML:'', style:{display:'none',setProperty(name,value){this[name]=value}},
    classList:{add(name){classes.add(name)},remove(name){classes.delete(name)},toggle(name,on){if(on)classes.add(name);else classes.delete(name)},contains(name){return classes.has(name)}},
    setAttribute(name,value){attrs[name]=String(value)},removeAttribute(name){delete attrs[name]},
    getAttribute(name){return attrs[name]},contains(other){return other===this}};
}
const nodes = Object.fromEntries(['contextMeter','contextMeterPopover','composerRuntimeDock'].map(id=>[id,element()]));
const document = {documentElement:{lang:'zh-CN'},getElementById:id=>nodes[id]||null,addEventListener(){}};
const c = {document, currentSessionId:'A', escHtml:value=>String(value)};
vm.createContext(c);
vm.runInContext(extract('const DESKTOP_I18N =', 'function resolvedLanguage('),c);
vm.runInContext(extract('function closeContextMeterPopover()', 'function renderStatusSnapshot(d)'),c);
const meter=nodes.contextMeter,pop=nodes.contextMeterPopover;
const snapshot={contextSessionId:'A',contextUsed:96000,contextWindow:128000,compactAtTokens:102400};
const render=data=>vm.runInContext('renderContextMeter('+JSON.stringify(data)+', DESKTOP_I18N[document.documentElement.lang])',c);
for (const [stage,label] of [['summarizing','生成摘要中'],['checking','校验压缩结果中'],['saving','保存压缩结果中']]) {
  render({...snapshot,contextCompactionStage:stage});
  assert.match(meter.innerHTML,/75%/,'percentage retains committed usage');
  assert.ok(meter.innerHTML.includes(label),'phase is visible without opening details');
  assert.ok(meter.getAttribute('aria-label').includes(label),'phase is accessible');
  assert.match(pop.innerHTML,/压缩前已用.*75%/,'in-progress occupancy is clearly pre-compaction');
  assert.match(pop.innerHTML,/96,000 tokens/);
  assert.ok(pop.innerHTML.includes(label),'details show the current phase');
  assert.match(pop.innerHTML,/自动压缩阈值.*102,400 tokens/,'threshold must retain its separate meaning');
  assert.doesNotMatch(pop.innerHTML,/压缩成功|压缩完成/,'an uncommitted result cannot claim success');
}
render({...snapshot,contextUsed:16000});
assert.match(meter.innerHTML,/13%/,'committed result replaces the old usage');
assert.doesNotMatch(meter.innerHTML,/生成摘要|校验压缩|保存压缩/);
assert.match(pop.innerHTML,/上下文已用.*13%/);
assert.doesNotMatch(pop.innerHTML,/压缩前已用|生成摘要|校验压缩|保存压缩/);
render({...snapshot,contextCompactionStage:'saving'});
c.currentSessionId='B';render(snapshot);
assert.equal(meter.style.display,'none','another session cannot show active compression');
assert.equal(pop.hidden,true);
render({contextSessionId:'B',contextUsed:1000,contextWindow:128000});
assert.doesNotMatch(meter.innerHTML,/保存压缩/,'session switch does not retain the old phase');
assert.doesNotMatch(pop.innerHTML,/压缩前已用|保存压缩/);
c.currentSessionId='A';document.documentElement.lang='en';
render({...snapshot,contextCompactionStage:'saving'});
assert.match(meter.innerHTML,/Saving compacted context/);
assert.match(pop.innerHTML,/Used before compaction.*75%/);
assert.match(pop.innerHTML,/Saving compacted context/);
render({...snapshot,contextCompactionStage:'unexpected'});
assert.doesNotMatch(pop.innerHTML,/undefined|Used before compaction|unexpected/,'unknown phases must not become user-facing text');
`, "app.js")
}
