package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

// Exercise the persisted-message index without a renderer stub: image inputs
// need their own virtual turn before any of that turn's DOM is mounted.
func TestHistoryVirtualImageTurnGrouping(t *testing.T) {
	runImageHistoryVirtualCase(t, `
const image=()=>({type:'image',media_type:'image/png',data:'aW1hZ2U=',attachment_hash:'fixture'});
const history=[
  {role:'user',content:'<private>hidden</private>'},
  {role:'user',content:'first question'},
  {role:'assistant',content:[{type:'text',text:'working'},{type:'tool_use',name:'Read',tool_use_id:'read-1'}]},
  {role:'user',content:[{type:'tool_result',tool_use_id:'read-1',content:[image()]}]},
  {role:'assistant',content:'first answer'},
  {role:'user',content:[image()]},
  {role:'assistant',content:'image answer'},
  {role:'user',synthetic:true,content:[{type:'text',text:'runtime instruction'},image()]},
  {role:'user',content:[{type:'text',text:'synthetic block',synthetic:true},{...image(),synthetic:true}]},
  {role:'user',content:'[user steer mid-turn] correction'},
  {role:'assistant',content:'steered answer'},
  {role:'user',content:[{type:'text',text:'describe both'},image(),image()]},
  {role:'assistant',content:'combined answer'},
];
const visibleText=text=>String(text).replace(/<private>[\s\S]*?<\/private>/g,'');
const build=messages=>c.buildHistoryTurnIndex(messages,'image-session',{visibleText});
const units=build(history);
assert.equal(units.find(unit=>unit.entries.includes(history[5])).sourceStart,5,
  'the image-only source message must open its own turn instead of joining the preceding answer');
assert.equal(units.length,4,'hidden preamble plus three actual user inputs define four virtual units');
assert.deepEqual(Array.from(units,unit=>unit.turn),[0,1,2,3]);
const imageTurn=units[2],mixedTurn=units[3];
assert.equal(imageTurn.sourceStart,5,'the image-only source message must open its own turn');
assert.equal(imageTurn.sourceEnd,11,'synthetic input, tool results and mid-turn steer stay with their owning turn');
assert.deepEqual(Array.from(imageTurn.entrySourceIndices),[5,6,7,8,9,10]);
assert.equal(mixedTurn.sourceStart,11,'text and images in one persisted message create only one turn');
assert.equal(mixedTurn.sourceEnd,13);
assert.equal(imageTurn.textIndexStart,3,'images do not consume visible text indices');
assert.equal(mixedTurn.textIndexStart,8,'visible synthetic rows and steer retain the existing text counter');
assert.equal(mixedTurn.textIndexEnd,10);
assert.equal(units[1].sourceStart,1);assert.equal(units[1].sourceEnd,5);
const flattened=units.flatMap(unit=>unit.entries);
assert.equal(flattened.length,history.length,'no persisted source message may be discarded');
for(let index=0;index<history.length;index++)assert.equal(flattened[index],history[index]);
const appended=build([...history,{role:'user',content:[image()]}]);
for(let index=0;index<units.length;index++){
  assert.equal(appended[index].key,units[index].key,'later image turns preserve earlier reading keys');
  assert.equal(appended[index].signature,units[index].signature,'later image turns preserve completed source signatures');
}
`)
}

func TestHistoryVirtualImageHeightEstimate(t *testing.T) {
	runImageHistoryVirtualCase(t, `
const image=()=>({type:'image',media_type:'image/png',data:'aW1hZ2U='});
const build=content=>c.buildHistoryTurnIndex([{role:'user',content}],'image-height')[0];
const single=build([image()]),pair=build([image(),image()]),grid=build(Array.from({length:7},image));
assert(single.estimatedHeight>=180&&single.estimatedHeight<=280,
  'one image reserves its full preview row before the image load event');
assert(pair.estimatedHeight>=112&&pair.estimatedHeight<single.estimatedHeight,
  'two thumbnails share one compact image row instead of estimating two large previews');
assert(grid.estimatedHeight>=3*112&&grid.estimatedHeight<=4*160,
  'a larger thumbnail grid reserves multiple rows within the two-to-three-column layout');
const mixed=build([{type:'text',text:'describe this'},image()]);
assert(mixed.estimatedHeight>single.estimatedHeight,'caption text adds space beside the image row estimate');
const nested=build([{type:'tool_result',tool_use_id:'read-image',content:[image()]}]);
assert.equal(nested.estimatedHeight,1,'nested tool images are not standalone user thumbnail rows');
const sourceIndex=[single,pair,grid].map((unit,index)=>({...unit,key:'turn-'+index}));
const top=c.historyVirtualRange(sourceIndex,0,100,0);
assert.equal(top.start,0);assert.equal(top.end,1,
  'an unloaded full-size image occupies the first viewport instead of exposing following turns');
`)
}

func runImageHistoryVirtualCase(t *testing.T, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/history_virtual.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `const assert=require('node:assert/strict'),vm=require('node:vm');
const c=vm.createContext({});
vm.runInContext(require('node:fs').readFileSync(0,'utf8'),c);
` + body
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("image history virtual index: %v\n%s", err, output)
	}
}
