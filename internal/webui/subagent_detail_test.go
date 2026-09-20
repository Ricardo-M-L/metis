package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestSubAgentDetailServesLiveAndRetainedOutput(t *testing.T) {
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	teammate := &agent.Teammate{
		Name:       "transport",
		AgentID:    "agt-transport",
		Background: true,
		Started:    time.Now().Add(-2 * time.Second),
	}
	teammate.AppendText("reading the runtime\n")
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})

	read := func(method string) (int, http.Header, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		server.handler().ServeHTTP(rr, httptest.NewRequest(method, "/api/subagents/agt-transport", nil))
		body := map[string]any{}
		if rr.Body.Len() > 0 {
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v\n%s", err, rr.Body.String())
			}
		}
		return rr.Code, rr.Header(), body
	}

	code, _, payload := read(http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d: %#v", code, payload)
	}
	view, _ := payload["agent"].(map[string]any)
	if got := view["name"]; got != "transport" {
		t.Fatalf("name = %#v", got)
	}
	if got := view["status"]; got != "running" {
		t.Fatalf("status = %#v", got)
	}
	if got := view["output"]; got != "reading the runtime\n" {
		t.Fatalf("live output = %#v", got)
	}
	if got := view["background"]; got != true {
		t.Fatalf("background = %#v", got)
	}

	// The roster keeps completed entries briefly specifically so the Desktop
	// can still open an agent whose work finished while its status menu was up.
	teammate.Finish(agent.StatusFailed, "partial final result", errors.New("provider timeout"), "timeout 30s")
	roster.UnregisterTeammate(teammate)
	code, _, payload = read(http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("GET retained detail = %d: %#v", code, payload)
	}
	view, _ = payload["agent"].(map[string]any)
	for key, want := range map[string]string{
		"status":    "failed",
		"result":    "partial final result",
		"stopHint":  "timeout 30s",
		"exitError": "provider timeout",
	} {
		if got := view[key]; got != want {
			t.Fatalf("retained %s = %#v, want %q", key, got, want)
		}
	}

	code, headers, _ := read(http.MethodPost)
	if code != http.StatusMethodNotAllowed || headers.Get("Allow") != http.MethodGet {
		t.Fatalf("POST detail = %d Allow=%q, want 405 GET", code, headers.Get("Allow"))
	}
}

func TestSubAgentDetailOutputBoundPreservesHeadAndTail(t *testing.T) {
	value := "开头-" + strings.Repeat("x", subAgentDetailOutputLimit+20) + "-结尾"
	got, truncated := trimSubAgentDetailOutput(value)
	if !truncated {
		t.Fatal("large output was not marked truncated")
	}
	if !strings.HasPrefix(got, "开头-") || !strings.HasSuffix(got, "-结尾") {
		t.Fatalf("bounded output did not preserve both ends: %q…%q", got[:min(20, len(got))], got[max(0, len(got)-20):])
	}
	if !strings.Contains(got, "METIS truncated this sub-agent output") {
		t.Fatalf("bounded output has no truncation marker: %q", got)
	}
}

func TestSubAgentDetailStreamEmitsSnapshotDeltaAndTerminal(t *testing.T) {
	_, store := testServer(t)
	roster := agent.NewRoster(2)
	teammate := &agent.Teammate{
		Name:       "transport",
		AgentID:    "agt-transport",
		Background: true,
		Started:    time.Now().Add(-time.Second),
	}
	teammate.AppendText("initial ")
	if err := roster.Register(teammate); err != nil {
		t.Fatal(err)
	}
	server := NewServer("127.0.0.1:0", nil, store, RuntimeBindings{Roster: roster})
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/subagents/agt-transport/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("stream Content-Type = %q", got)
	}
	reader := bufio.NewReader(resp.Body)

	event, payload := nextSubAgentSSE(t, reader)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	view, _ := payload["agent"].(map[string]any)
	if got := view["output"]; got != "initial " {
		t.Fatalf("snapshot output = %#v", got)
	}

	teammate.AppendText("live text")
	event, payload = nextSubAgentSSE(t, reader)
	if event != "delta" || payload["delta"] != "live text" {
		t.Fatalf("live SSE = %q %#v, want delta", event, payload)
	}

	teammate.Finish(agent.StatusCompleted, "initial live text", nil, "end_turn")
	event, payload = nextSubAgentSSE(t, reader)
	if event != "terminal" {
		t.Fatalf("terminal event = %q, want terminal", event)
	}
	view, _ = payload["agent"].(map[string]any)
	if got := view["status"]; got != "completed" {
		t.Fatalf("terminal status = %#v", got)
	}
	if got := view["result"]; got != "initial live text" {
		t.Fatalf("terminal result = %#v", got)
	}
}

func nextSubAgentSSE(t *testing.T, reader *bufio.Reader) (string, map[string]any) {
	t.Helper()
	type received struct {
		event   string
		payload map[string]any
		err     error
	}
	result := make(chan received, 1)
	go func() {
		var event string
		var data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				result <- received{err: err}
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if event == "" || data == "" {
					continue
				}
				payload := map[string]any{}
				if err := json.Unmarshal([]byte(data), &payload); err != nil {
					result <- received{err: err}
					return
				}
				result <- received{event: event, payload: payload}
				return
			}
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case received := <-result:
		if received.err != nil {
			t.Fatalf("read sub-agent SSE: %v", received.err)
		}
		return received.event, received.payload
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for sub-agent SSE")
	}
	return "", nil
}

func TestDesktopSubAgentPanelLivesInHeaderAndLoadsOutput(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(index)
	headerStart := strings.Index(page, `id="viewTabs"`)
	chatStart := strings.Index(page, `id="chatArea"`)
	statusStart := strings.Index(page, `<div class="status-row">`)
	inputStart := strings.Index(page, `<div class="input-area">`)
	if headerStart < 0 || chatStart <= headerStart || !strings.Contains(page[headerStart:chatStart], `id="agentStatusAnchor"`) {
		t.Fatal("sub-agent status trigger is not mounted in the conversation header")
	}
	if statusStart < 0 || inputStart <= statusStart {
		t.Fatal("cannot locate legacy status row")
	}
	if strings.Contains(page[statusStart:inputStart], `id="statusChip"`) {
		t.Fatal("sub-agent status trigger is still mounted above the composer")
	}
	for _, want := range []string{`id="subAgentDetailOverlay"`, `id="subAgentDetailBody"`, `aria-modal="true"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("sub-agent detail markup missing %q", want)
		}
	}

	source, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(source)
	for _, want := range []string{"new window.EventSource('/api/subagents/'", "addEventListener('delta'", "subAgentDetailStream"} {
		if !strings.Contains(js, want) {
			t.Fatalf("sub-agent live stream wiring missing %q", want)
		}
	}
	start := strings.Index(js, "function closeStatusPopover()")
	endOffset := -1
	if start >= 0 {
		endOffset = strings.Index(js[start:], "// ============================================================\n// Layout")
	}
	end := start + endOffset
	if start < 0 || end <= start {
		t.Fatal("cannot isolate sub-agent Desktop interaction code")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// Execute the real panel code with a small browser-shaped harness. This
	// proves that selecting a roster row opens the dialog and requests the
	// live output endpoint, rather than merely asserting source strings.
	const harness = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
class Element {
  constructor(){ this.style={display:'none'}; this.dataset={}; this.attrs={}; this.hidden=true; this.textContent=''; this.innerHTML=''; this.children=[]; this.listeners={}; this.classList={add(){},remove(){},toggle(){}}; }
  setAttribute(k,v){ this.attrs[k]=String(v); }
  appendChild(v){ this.children.push(v); return v; }
  append(...vs){ this.children.push(...vs); }
  addEventListener(k,fn){ (this.listeners[k] ||= []).push(fn); }
  querySelector(){ return this.dialog ||= new Element(); }
  querySelectorAll(){ return []; }
  focus(){ this.focused=true; }
}
const byId = new Map();
for (const id of ['statusPopover','statusChip','subAgentDetailOverlay','subAgentDetailTitle','subAgentDetailDescription','subAgentDetailBody']) byId.set(id,new Element());
const overlay = byId.get('subAgentDetailOverlay'); overlay.dialog = new Element();
const c = {
  console, Promise, Map, Set, encodeURIComponent, requestAnimationFrame: fn => fn(),
  subAgentDetailState:{agentId:'',trigger:null,data:null,loading:false,error:'',requestGeneration:0},
  subAgentDetailStream:null, subAgentDetailStreamGeneration:0,
  document:{documentElement:{lang:'zh-CN'}, body:new Element(), getElementById:id=>byId.get(id)||null, createElement:()=>new Element(), addEventListener(){}},
  uiText:(en,zh)=>zh, escHtml:v=>String(v),
  fetch:async url=>({ok:true,json:async()=>({agent:{name:'transport',agentId:'agt-transport',status:'running',background:true,elapsedMs:2400,output:'正在读取 transport.go'}})}),
};
c.window=c; vm.createContext(c); vm.runInContext(source,c);
(async()=>{
  c.openSubAgentDetails('agt-transport', byId.get('statusChip'));
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(overlay.hidden,false);
  assert.equal(byId.get('subAgentDetailTitle').textContent,'transport');
  assert.match(byId.get('subAgentDetailDescription').textContent,/运行中/);
  assert.match(byId.get('subAgentDetailBody').children.at(-1).children.at(-1).textContent,/transport.go/);
})().catch(error=>{console.error(error);process.exitCode=1;});
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Stdin = bytes.NewBufferString(js[start:end])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sub-agent Desktop interaction: %v\n%s", err, out)
	}
}
