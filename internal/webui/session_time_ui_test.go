package webui

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestSessionTimeUIRejectsZeroDatesAcrossActivationAndReload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/sessions.js")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(string(source))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const now = Date.parse('2026-10-30T12:00:00Z');
class FixedDate extends Date {
  constructor(...args) { super(...(args.length ? args : [now])); }
  static now() { return now; }
}
function extract(start, next) {
  const from=source.indexOf(start), to=source.indexOf(next, from+start.length);
  assert(from>=0 && to>from, start);
  return source.slice(from,to);
}
const c={Date:FixedDate, URLSearchParams, sessions:[], currentSessionId:'A', desktopPreferences:{sidebarSort:'updated'},
  sessionsLoading:false,sessionsLoadGeneration:0,sessionsNextCursor:'',sessionsTotal:0,sessionFilter:'',showArchivedSessions:false,
  uiText:en=>en, escAttr:String,escHtml:String,escOnclick:String,
  sessionState:()=>({name:'idle',label:'Idle'}),sessionDisplayTitle:s=>s.title||'Untitled',sessionStatusIcon:()=>'',
  document:{getElementById:()=>({innerHTML:''})}, renderSessions(){},
};
vm.createContext(c);
for(const [start,next] of [
  ['function mergeServerSessionMetadata(session)','function isWorkspaceSelectionPending()'],
  ['function relativeTime(ts)','let sessionsExpanded = false;'],
  ['function sortSessionItems(items)','function workspaceLabel('],
  ['function renderSessionItem(s)','function renderSessions('],
  ['async function loadSessions(append)','async function loadMoreSessions('],
]) vm.runInContext(extract(start,next),c);
const zero='0001-01-01T00:00:00Z', created='2026-10-28T12:00:00Z', updated='2026-10-30T11:58:00Z';
const timeLabel=s=>c.renderSessionItem(s).match(/class="session-item-time">([^<]*)</)[1];
const detail=s=>c.renderSessionItem(s).match(/data-detail-meta="([^"]*)"/)[1];
const plain=v=>JSON.parse(JSON.stringify(v));
(async()=>{
  // Activation replies may omit fields or serialize Go zero values. Neither
  // can erase a valid timestamp from the previously fetched list.
  c.sessions=[{id:'A',title:'Before',createdAt:created,updatedAt:updated}];
  c.mergeServerSessionMetadata({id:'A',title:'After',createdAt:zero,updatedAt:zero});
  assert.equal(c.sessions[0].createdAt,created);
  assert.equal(c.sessions[0].updatedAt,updated);
  assert.equal(c.sessions[0].title,'After');
  assert.equal(timeLabel(c.sessions[0]),'2 mins');
  c.mergeServerSessionMetadata({id:'A',updatedAt:null});
  c.mergeServerSessionMetadata({id:'A',updatedAt:'not-a-date',createdAt:''});
  c.mergeServerSessionMetadata({id:'A',status:'completed'});
  assert.equal(c.sessions[0].updatedAt,updated);
  assert.equal(c.sessions[0].createdAt,created);

  // With no cache, zero or malformed updates fall back to the valid creation
  // instant. The tooltip must describe that same instant, never Invalid Date.
  const fallback={id:'B',title:'Fallback',createdAt:created,updatedAt:zero};
  assert.equal(timeLabel(fallback),'2 days');
  assert(detail(fallback).includes(new FixedDate(created).toLocaleString()));
  const invalid={id:'C',title:'Unknown',createdAt:zero,updatedAt:'invalid'};
  assert.equal(timeLabel(invalid),'');
  assert.doesNotMatch(detail(invalid),/Invalid Date|0001|1970/);
  c.mergeServerSessionMetadata(invalid);
  assert.equal(c.sessions.find(s=>s.id==='C').updatedAt,undefined);
  assert.equal(c.sessions.find(s=>s.id==='C').createdAt,undefined);

  // The same validity rules govern both normal and manual-order tie sorting.
  const items=[invalid,fallback,{id:'D',createdAt:'2026-10-29T12:00:00Z',updatedAt:null},{id:'E',createdAt:created,updatedAt:updated}];
  assert.deepEqual(plain(c.sortSessionItems(items).map(s=>s.id)),['E','D','B','C']);
  assert.deepEqual(items.map(s=>s.id),['C','B','D','E'],'sorting must not mutate server data');
  c.desktopPreferences={sidebarSort:'manual',sessionOrder:['B']};
  assert.deepEqual(plain(c.sortSessionItems(items).map(s=>s.id)),['B','E','D','C']);
  c.desktopPreferences={sidebarSort:'name'};
  assert.deepEqual(plain(c.sortSessionItems([{id:'Z',title:'Zulu'},{id:'A',title:'Alpha'}]).map(s=>s.id)),['A','Z']);

  // Bad scalar values must never become epoch dates; a valid old conversation
  // and a future instant retain their real metadata rather than being reset.
  for(const value of [undefined,null,'','  ',zero,'0000-01-01T00:00:00Z','invalid',NaN,Infinity,false,[],{}]) {
    assert.equal(c.relativeTime(value),'',String(value));
  }
  const historical={id:'old',createdAt:'2020-01-01T12:00:00Z'};
  const expectedDays=Math.floor((now-Date.parse(historical.createdAt))/86400000);
  assert.equal(timeLabel(historical),expectedDays+' days');
  assert(detail(historical).includes(new FixedDate(historical.createdAt).toLocaleString()));
  const future={id:'future',createdAt:created,updatedAt:'2026-10-31T12:00:00Z'};
  assert.equal(timeLabel(future),'just now');
  c.mergeServerSessionMetadata(future);
  assert.equal(c.sessions.find(s=>s.id==='future').updatedAt,future.updatedAt);

  // Exercise the actual list-loading code around activation and pagination.
  // A legacy list with zero dates must not poison a valid cached timestamp.
  c.desktopPreferences={sidebarSort:'updated'};
  c.sessions=[{id:'A',createdAt:created,updatedAt:updated}];
  c.fetch=async()=>({ok:true,json:async()=>({sessions:[{id:'A',createdAt:created,updatedAt:zero}],total:1})});
  await c.loadSessions(false);
  assert.equal(c.sessions[0].updatedAt,updated);
  c.mergeServerSessionMetadata({id:'A',createdAt:zero,updatedAt:zero});
  assert.equal(timeLabel(c.sessions[0]),'2 mins');
  const nextUpdate='2026-10-30T11:59:00Z';
  c.fetch=async()=>({ok:true,json:async()=>({sessions:[{id:'A',createdAt:created,updatedAt:nextUpdate},{id:'B',createdAt:created,updatedAt:zero}],total:2})});
  await c.loadSessions(true);
  assert.equal(c.sessions.find(s=>s.id==='A').updatedAt,nextUpdate,'valid authoritative updates still apply');
  assert.equal(timeLabel(c.sessions.find(s=>s.id==='A')),'1 min');
  assert.equal(timeLabel(c.sessions.find(s=>s.id==='B')),'2 days');
  assert.equal(c.sessions.length,2);
  console.log('session time regression checks passed');
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session time regression checks: %v\n%s", err, output)
	}
}
