package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

// Exercise the real UI's asynchronous behavior without a browser or network.
func TestUsageStatisticsUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/usage_stats.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert=require('node:assert/strict'),vm=require('node:vm');
const source=require('node:fs').readFileSync(0,'utf8');
const pending=[];
const content={innerHTML:'',insertAdjacentHTML(_p,s){this.innerHTML=s+this.innerHTML}};
const overlay={hidden:false}, focused=[];
const document={documentElement:{lang:'zh-CN'},getElementById(id){return id==='settingsContent'?content:id==='settingsOverlay'?overlay:(id.startsWith('usagePeriod')||id.startsWith('usageHeat'))?{focus(){focused.push(id)}}:null}};
const esc=x=>String(x).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const c={document,console,Intl,Date,Map,Number,String,Math,Array,Promise,settingsTab:'usage',escHtml:esc,escAttr:esc,
 uiText:(en,zh)=>document.documentElement.lang==='zh-CN'?zh:en,
 fetch(url){assert.equal(url,'/api/usage/statistics');return new Promise(resolve=>pending.push(resolve));}};
vm.createContext(c);vm.runInContext(source,c);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const today=c.usageLocalDate(new Date()), earlier=c.usageLocalDate(c.usageDayOffset(new Date(),-15));
const data={scope:'local',total:{inputTokens:13,outputTokens:3,cacheReadTokens:2,cacheCreateTokens:1,totalTokens:16},
 daily:[{date:today,totalTokens:11,turns:1,durationMs:1000},{date:earlier,totalTokens:5,turns:1,durationMs:1000}],coverage:{sessions:2,sessionsWithUsage:1,sessionsWithoutUsage:1,unattributedTokens:9,inconsistentSessions:1,invalidMetricTurns:1,metricOnlySessions:1,unreadableSessions:1,modelInconsistentSessions:1,modelUnreadableSessions:1,unattributedModelTokens:8,sessionsWithDuration:1,invalidDurationTurns:1},
 longestTurnMs:1000,longestSessionMs:1000,currentStreak:1,longestStreak:1,peakDayTokens:11,modelBreakdownAvailable:false};
(async()=>{
 c.renderUsageStatistics(); assert.match(content.innerHTML,/正在读取本机使用统计/); assert.equal(pending.length,1);
 overlay.hidden=true; c.settingsTab='general'; pending.shift()({ok:true,json:async()=>data}); await tick();
 assert.match(content.innerHTML,/正在读取本机使用统计/,'a delayed request must not overwrite another page');
 overlay.hidden=false;c.settingsTab='usage';c.renderUsageStatistics();
 assert.match(content.innerHTML,/使用统计/);assert.match(content.innerHTML,/累计 Token 数/);assert.match(content.innerHTML,/16/);assert.match(content.innerHTML,/最长会话/);assert.match(content.innerHTML,/1 秒/);
 assert.match(content.innerHTML,/usage-heat-cell is-recorded" data-level="4"/,"dated data must appear in 52-week heatmap");
 assert.match(content.innerHTML,/usage-trend-bar" style="height:100%"/,"dated data must appear in daily trend");
 assert.match(content.innerHTML,new RegExp('tabindex="0" role="img" aria-label="' + today + ' · 11 tokens · 1 次运行"'),'recorded bars must expose date, tokens and turn count to keyboard and screen readers');
 assert.match(content.innerHTML,new RegExp('<span>' + today + '</span><strong>11 Token</strong>'),'hover tooltip must show the actual token count');
 assert.match(content.innerHTML,/usage-trend-tooltip[^>]*aria-hidden="true"/,'visible tooltip must not repeat the accessible label');
 const withModel=c.usageStatisticsMarkup({...data,models:[{model:"<bad>",totalTokens:11}],modelBreakdownAvailable:true});assert.match(withModel,/&lt;bad&gt;/);assert.doesNotMatch(withModel,/<bad>/);
 assert.match(content.innerHTML,/无法归属日期/);assert.match(content.innerHTML,/无记录/);assert.match(content.innerHTML,/逐轮数据计入下限/);
 assert.match(content.innerHTML,/无法读取/);assert.match(content.innerHTML,/模型记录与逐轮明细或汇总不一致/);assert.match(content.innerHTML,/缺少有效时长或超出数值范围/);assert.match(content.innerHTML,/缺少模型归属/);assert.match(content.innerHTML,/仅基于有日期/);assert.match(content.innerHTML,/暂无法按模型统计/);
 const metricOnly=c.usageStatisticsMarkup({...data,coverage:{...data.coverage,sessionsWithUsage:0,metricOnlySessions:1}});assert.match(metricOnly,/usage-metric-value">16</,"metric-only recorded totals must remain visible");
 assert.match(content.innerHTML,/aria-label="滚动查看每日活动"/);assert.match(content.innerHTML,/以表格查看 52 周活动/);assert.doesNotMatch(content.innerHTML,/以表格查看有记录的日期/);
 c.setUsageStatisticsPeriod(30); assert.match(content.innerHTML,/aria-pressed="true" onclick="setUsageStatisticsPeriod\(30\)/);assert.equal(focused.at(-1),'usagePeriod30','keyboard focus should survive period redraw');
 c.setUsageHeatMode('weekly');assert.equal(focused.at(-1),'usageHeatWeekly');assert.match(content.innerHTML,/aria-pressed="true" onclick="setUsageHeatMode\('weekly'\)/);assert.match(content.innerHTML,/5 tokens/);assert.match(content.innerHTML,/11 tokens/);assert.match(content.innerHTML,/aria-pressed="true" onclick="setUsageStatisticsPeriod\(30\)/,'heatmap mode must not reset trend range');
 c.setUsageHeatMode('cumulative');assert.equal(focused.at(-1),'usageHeatCumulative');assert.match(content.innerHTML,/已记录累计/);assert.match(content.innerHTML,/16 tokens/);assert.match(content.innerHTML,/本周无新增记录/);assert.match(content.innerHTML,/截至 .+ 的已记录累计：无记录/,'unknown history is not displayed as zero');
 c.setUsageHeatMode('daily');assert.equal(focused.at(-1),'usageHeatDaily');
 c.refreshUsageStatistics();assert.match(content.innerHTML,/正在刷新/);assert.equal(pending.length,1);
 c.refreshUsageStatistics();assert.equal(pending.length,2);
 const first=pending.shift(),second=pending.shift();second({ok:true,json:async()=>({...data,total:{...data.total,totalTokens:99}})});await tick();
 first({ok:true,json:async()=>({...data,total:{...data.total,totalTokens:1}})});await tick();assert.match(content.innerHTML,/>99</,'late refresh replaced newer result');
 document.documentElement.lang='en';c.renderUsageStatistics();assert.match(content.innerHTML,/Usage statistics/);assert.match(content.innerHTML,/Per|model/);
 c.refreshUsageStatistics();pending.shift()({ok:false,status:503});await tick();assert.match(content.innerHTML,/Request failed \(503\)/);assert.match(content.innerHTML,/>99</,'failed refresh removed previous data');
 console.log('usage UI behavior ok');
})().catch(err=>{console.error(err);process.exitCode=1});`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("usage UI: %v\n%s", err, out)
	}
}
