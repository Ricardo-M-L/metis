package webui

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestSessionFileSourceRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	sources := map[string]string{}
	for name, path := range map[string]string{
		"prism":  "static/vendor/prism/prism.min.js",
		"render": "static/session_file_render.js",
	} {
		contents, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(contents)
	}
	payload, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const {prism, render} = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const decode = value => value.replace(/<\/?span\b[^>]*>/g, '').replace(/&(?:amp|lt|gt|quot|#39);/g, s => ({'&amp;':'&','&lt;':'<','&gt;':'>','&quot;':'"','&#39;':"'"})[s]);
class Element {
  constructor(tag) { this.tagName=tag; this.children=[]; this.dataset={}; this.attrs={}; this.textContent=''; }
  append(...items) { this.children.push(...items); }
  setAttribute(name,value) { this.attrs[name]=value; }
}
const c = {document:{createElement:tag=>new Element(tag),currentScript:{tagName:'SCRIPT',src:'vendor/prism/prism.min.js',hasAttribute:()=>false}}, uiText:en=>en};
c.window = c;
vm.createContext(c);
vm.runInContext(prism, c, {timeout:3000});
vm.runInContext(render, c, {timeout:3000});
assert.equal(c.Prism.manual, true, 'vendor must not auto-highlight the surrounding chat');
for (const [file, expected] of [
  ['a.sh','bash'], ['.zshrc','bash'], ['/a/.env.local','bash'], ['main.go','go'],
  ['app.ts','typescript'], ['app.tsx','typescript'], ['APP.MJS','javascript'],
  ['package.json','json'], ['compose.yaml','yaml'], ['README.MD','markdown'],
  ['Dockerfile.dev','docker'], ['go.mod','go'], ['Cargo.lock','toml'],
  ['app.py','python'], ['app.rs','rust'], ['style.css','css'], ['icon.svg','markup'],
  ['report.txt','plaintext'], ['opaque.unknown','plaintext'], ['__proto__','plaintext'],
]) assert.equal(c.sessionFileLanguage({path:file}).id, expected, file);
assert.equal(c.sessionFileLanguage({name:'report', kind:'markdown'}).id,'markdown');
assert.equal(c.sessionFileLanguage({name:'a.ts',path:'/a/a.txt'}).label,'TypeScript');
assert.equal(c.sessionFileLanguage('C:\\source\\a.go').id,'go');

function verify(content, file, highlighted=true) {
  const lines = c.sessionFileHighlightedLines(content,{name:file});
  assert.equal(Array.from(lines,decode).join('\n'),content.replace(/\r\n?/g,'\n'),file+' content loss');
  for(const line of lines) {
    assert.equal((line.match(/<span /g)||[]).length,(line.match(/<\/span>/g)||[]).length,'unbalanced line');
    assert(!line.replace(/<span class="token [a-z\d -]+">|<\/span>/gi,'').includes('<'),'unsafe markup: '+line);
  }
  assert.equal(lines.some(line=>line.includes('<span class="token ')),highlighted,file+' tokenization');
  return lines;
}
verify('package main\n\nfunc main() { println("hello") }\n','main.go');
verify('#!/bin/bash\nset -eu\nprintf "%s\\n" "$HOME"\n','run.sh');
verify('const title: string = "<&>";\n// <img src=x onerror=alert(1)>\n','app.ts');
verify('{"key":"<script>alert(1)</script>","n":42,"ok":true}\n','a.json');
verify('name: "<&>"\nactive: true\n# comment\n','a.yaml');
verify('# Heading\n\n**bold** and [x](javascript:alert(1))\n<script>alert(1)</script>\n','a.md');
verify('<svg onload="alert(1)"><script>alert(2)</script></svg>\n','a.svg');
verify('<script>alert(1)</script> & "\'\t中文\r\nnext\rline\n','a.unknown',false);
verify('', 'empty.txt', false);
verify('\n', 'blank.txt', false);
const comments=verify('/* first\n second <img src=x>\n third */\nconst x = 1;\n','multiline.ts');
assert(comments[0].includes('token comment') && comments[1].includes('token comment') && comments[2].includes('token comment'));
const source=c.renderSessionFileSource('const a = 1;\r\n\t中文\n',{name:'app.ts'},{wrap:true});
assert.equal(source.className,'session-file-code is-wrapped');
assert.equal(source.dataset.language,'typescript');
assert.equal(source.dataset.limited,'false');
assert.equal(source.tabIndex,0);
assert.equal(source.children.length,3,'trailing newline has its own line');
assert.equal(source.children[1].className,'session-file-code-line');
assert.equal(source.children[1].children[0].textContent,'2');
assert.equal(source.children[1].children[0].attrs['aria-hidden'],'true');
assert.equal(source.children[1].children[1].className,'session-file-line-text');
assert.equal(decode(source.children[1].children[1].innerHTML),'\t中文');
assert.equal(source.children[2].children[1].innerHTML,'');

// Keep large/minified text readable without feeding it to regex grammars.
const tokenize=c.Prism.tokenize; let calls=0;
c.Prism.tokenize=(...args)=>{calls++;return tokenize(...args);};
verify('a'.repeat(3000), 'minified.ts', false);
verify(('const x = 1;\n').repeat(9000), 'large.ts', false);
assert.equal(calls,0,'large input must bypass syntax parsing');
const many=c.renderSessionFileSource('\n'.repeat(13000),{name:'a.txt'});
assert.equal(many.dataset.limited,'true');
assert.equal(many.children.length,12001,'bounded line elements plus visible notice');
assert.equal(many.children[12000].className,'session-file-source-limit');
assert(many.children[12000].textContent.includes('12,000'));
const giant=c.renderSessionFileSource('a'.repeat(512*1024+1),{name:'a.txt'});
assert.equal(giant.children[0].children[1].innerHTML.length,512*1024);
assert.equal(giant.dataset.limited,'true');
const surrogate=c.sessionFileSourceModel('a'.repeat(512*1024-1)+'😀');
assert.equal(surrogate.text.length,512*1024-1);
assert.equal(surrogate.limited,true);

// Highlighter failure or absence never makes a source file unreadable.
c.Prism.tokenize=()=>{throw new Error('grammar unavailable');};
verify('const a = "<img src=x>";', 'fallback.ts', false);
c.Prism.tokenize=()=>[{type:'keyword" onclick="alert(1)',alias:['safe','<img>'],content:'<script>alert(1)</script>'}];
const forged=c.sessionFileHighlightedLines('<script>alert(1)</script>',{name:'forged.ts'}).join('');
assert(!forged.includes('onclick') && !forged.includes('<script') && !forged.includes('<img'));
assert.equal(decode(forged),'<script>alert(1)</script>');
c.Prism=undefined;
verify('const a = 1;', 'offline.ts', false);
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session file source rendering: %v\n%s", err, out)
	}
}

func TestSessionFileSourceAssetsServedOffline(t *testing.T) {
	s, _ := testServer(t)
	for path, marker := range map[string]string{
		"/session_file_render.js":       "function renderSessionFileSource(",
		"/vendor/prism/prism.min.js":    "PrismJS 1.30.0",
		"/vendor/prism/LICENSE":         "MIT LICENSE",
		"/icons/lucide/file-code-2.svg": "Lucide 1.45.0",
		"/icons/lucide/wrap-text.svg":   "Lucide 1.45.0",
	} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), marker) {
			t.Fatalf("offline source asset %s: status=%d missing %q", path, rr.Code, marker)
		}
	}
}
