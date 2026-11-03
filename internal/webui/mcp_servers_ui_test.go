package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

// Exercise the actual settings UI, including asynchronous navigation races.
func TestMcpServersSettingsUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/mcp_servers.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const source = require('node:fs').readFileSync(0,'utf8');
const pending = [], content={innerHTML:'',insertAdjacentHTML(_p,s){this.innerHTML=s+this.innerHTML}}, overlay={hidden:false};
const document={documentElement:{lang:'zh-CN'},getElementById(id){return id==='settingsContent'?content:id==='settingsOverlay'?overlay:null}};
const esc=x=>String(x).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const c={document,console,Intl,Date,Number,String,Array,Promise,settingsTab:'mcp',escHtml:esc,escAttr:esc,uiText:(en,zh)=>document.documentElement.lang==='zh-CN'?zh:en,
 fetch(url){assert.equal(url,'/api/mcp/servers');return new Promise(resolve=>pending.push(resolve));}};
vm.createContext(c);vm.runInContext(source,c);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const data={statusScope:'desktop-process',pluginScanIncomplete:true,servers:[{name:'<local>',source:'user',transport:'stdio',enabled:true,status:'registered',toolCount:3},{name:'plugin-tool',source:'plugin',transport:'http',enabled:true,status:'lazy',toolCount:0},{name:'disabled',source:'config',transport:'unknown',enabled:false,status:'disabled',toolCount:0},{name:'retained',source:'user',transport:'stdio',enabled:false,status:'registered',toolCount:2},{name:'unavailable',source:'runtime',transport:'unknown',enabled:true,status:'stopped',toolCount:0}]};
(async()=>{
 c.renderMcpServers();assert.match(content.innerHTML,/正在读取 MCP/);assert.equal(pending.length,1);
 overlay.hidden=true;c.settingsTab='general';pending.shift()({ok:true,json:async()=>data});await tick();assert.match(content.innerHTML,/正在读取 MCP/,'late request overwrote another page');
 overlay.hidden=false;c.settingsTab='mcp';c.renderMcpServers();
 assert.match(content.innerHTML,/MCP 服务器/);assert.match(content.innerHTML,/&lt;local&gt;/);assert.doesNotMatch(content.innerHTML,/<local>/);
 assert.match(content.innerHTML,/已注册/);assert.match(content.innerHTML,/发现 3 个工具/);assert.match(content.innerHTML,/用户配置/);assert.match(content.innerHTML,/配置传输：stdio/);assert.match(content.innerHTML,/配置传输：HTTP/);assert.match(content.innerHTML,/使用时连接/);assert.match(content.innerHTML,/已禁用/);assert.match(content.innerHTML,/未连接/);assert.match(content.innerHTML,/仍保留旧客户端或延迟句柄/);assert.match(content.innerHTML,/部分插件清单无法读取或验证/);assert.match(content.innerHTML,/未主动检测连接/);assert.doesNotMatch(content.innerHTML,/已连接/);
 c.refreshMcpServers();c.refreshMcpServers();assert.equal(pending.length,2);
 const first=pending.shift(),second=pending.shift();second({ok:true,json:async()=>({servers:[{name:'newer',enabled:true,status:'stopped'}]})});await tick();first({ok:true,json:async()=>({servers:[{name:'older'}]})});await tick();assert.match(content.innerHTML,/newer/);assert.doesNotMatch(content.innerHTML,/older/);
 c.refreshMcpServers();pending.shift()({ok:false,status:500,json:async()=>({error:'SECRET'})});await tick();assert.match(content.innerHTML,/无法刷新/);assert.match(content.innerHTML,/newer/);assert.doesNotMatch(content.innerHTML,/SECRET/);assert.equal(pending.length,0,'failed refresh must not loop');
 c.refreshMcpServers();pending.shift()({ok:true,json:async()=>({servers:[],pluginScanIncomplete:false})});await tick();assert.match(content.innerHTML,/尚未配置 MCP 服务器/);assert.doesNotMatch(content.innerHTML,/部分插件清单无法读取或验证/);
 document.documentElement.lang='en';c.renderMcpServers();assert.match(content.innerHTML,/No MCP servers configured/);
})().catch(e=>{console.error(e);process.exitCode=1});`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("MCP settings UI: %v\n%s", err, out)
	}
}
