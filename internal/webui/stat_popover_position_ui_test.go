package webui

import "testing"

func TestDesktopStatPopoversAnchorToTheirOwnTrigger(t *testing.T) {
	runDesktopStatsNode(t, `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'))['stat_popovers.js'];
const listeners = {}, observed = [], frames = [];
let disconnects=0;
const nodes = {};
const window = {innerWidth:1000,innerHeight:800,
  addEventListener(name,fn,options){listeners[name]={fn,options}},
  requestAnimationFrame(fn){frames.push(fn);return frames.length}};
const document = {documentElement:{clientWidth:1000,clientHeight:800},getElementById:id=>nodes[id]||null};
function trigger(left,top,width=150) {
  return {left,top,width,getBoundingClientRect(){return {left:this.left,top:this.top,width:this.width,height:26}},closest(){return nodes.container}};
}
function panel(width=320,height=240) {
  return {hidden:false,style:{},getBoundingClientRect(){return {
    width:Math.min(width,parseFloat(this.style.maxWidth)||width),
    height:Math.min(height,parseFloat(this.style.maxHeight)||height)}}};
}
const c={window,document,ResizeObserver:class {constructor(fn){this.fn=fn;c.resize=this}observe(n){observed.push(n)}disconnect(){disconnects++}}};
vm.createContext(c);vm.runInContext(source,c);
nodes.composerRuntimeDock={};nodes.container={};
nodes.sessionStatsTrigger=trigger(280,740);nodes.sessionStatsPopover=panel();
c.refreshComposerStatPopovers();
assert.equal(nodes.sessionStatsPopover.style.left,'280px','left edge follows selected pill');
assert.equal(nodes.sessionStatsPopover.style.top,'492px','panel bottom is 8px above the pill');
assert.ok(observed.includes(nodes.sessionStatsTrigger));
assert.ok(observed.includes(nodes.composerRuntimeDock));
assert.ok(observed.includes(nodes.container));
assert.equal(listeners.scroll.options,true,'nested scrolls trigger repositioning');
nodes.sessionStatsPopover.hidden=true;
nodes.tokenStatsTrigger=trigger(670,740);nodes.tokenStatsPopover=panel();
c.refreshComposerStatPopovers();
assert.equal(nodes.tokenStatsPopover.style.left,'668px','right edge stays 12px inside viewport');
nodes.tokenStatsTrigger.left=5;c.refreshComposerStatPopovers();
assert.equal(nodes.tokenStatsPopover.style.left,'12px');
nodes.tokenStatsTrigger.top=200;c.resize.fn();frames.shift()();
assert.equal(nodes.tokenStatsPopover.style.maxHeight,'180px','short windows scroll panel content');
assert.equal(nodes.tokenStatsPopover.style.top,'12px');
window.innerWidth=document.documentElement.clientWidth=300;
listeners.resize.fn();frames.shift()();
assert.equal(nodes.tokenStatsPopover.style.maxWidth,'276px');
assert.equal(nodes.tokenStatsPopover.style.left,'12px');
nodes.tokenStatsPopover.hidden=true;const prior=disconnects;c.refreshComposerStatPopovers();
assert.ok(disconnects>prior,'closing all panels disconnects size observers');
`, "stat_popovers.js")
}
