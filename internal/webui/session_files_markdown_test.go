package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestSessionFilesSharedMarkdownRemainsInert(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	chat, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	files, err := staticFS.ReadFile("static/session_files.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const src=require('node:fs').readFileSync(0,'utf8');
const escape=s=>String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;').replaceAll("'",'&#39;');
const c={window:{},escHtml:escape,escAttr:escape}; vm.createContext(c);
vm.runInContext(src.slice(0,src.indexOf('// SESSION FILE MARKDOWN TEST BOUNDARY')),c);
const chat=src.slice(src.indexOf('// SESSION FILE MARKDOWN TEST BOUNDARY'));
vm.runInContext(chat.slice(chat.indexOf('function formatContent('),chat.indexOf('// --- Input ---')),c);
const rendered=c.formatContent('# Report\n\n<script>alert(1)</script>\n\n![x](https://evil.test/pixel)\n\n[x](javascript:alert)');
assert(!/<script|<img|<iframe|href="javascript:/i.test(rendered));
assert(rendered.includes('&lt;script&gt;'));
const literal=c.inlineMd('\x60[x](/tmp/a.md)\x60');
assert(!literal.includes('data-session-file-path'));
assert(literal.includes('[x](/tmp/a.md)'));
const link=c.inlineMd('[报告](</tmp/中文 report.md:12>)');
assert(link.includes('data-session-file-path="/tmp/中文 report.md:12"'));
assert(!link.includes('href='));
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(append(append(files, []byte("\n// SESSION FILE MARKDOWN TEST BOUNDARY\n")...), chat...))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session file Markdown: %v\n%s", err, out)
	}
}
