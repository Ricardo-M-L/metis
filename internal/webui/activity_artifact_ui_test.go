package webui

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestArtifactCardsStayOutsideActivityGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/artifacts.js")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const assert = require('node:assert/strict');
const vm = require('node:vm');
const source = require('node:fs').readFileSync(0, 'utf8');
const start = source.indexOf('function renderArtifactPresentation(');
const end = source.indexOf('function renderArtifactGallery(', start);
assert(start >= 0 && end > start);

class Element {
  constructor(name, id = '') { this.className = name; this.dataset = { artifactId: id }; this.children = []; this.parentElement = null; }
  get classList() { return { contains: name => this.className.split(' ').includes(name) }; }
  get nextElementSibling() {
    if (!this.parentElement) return null;
    return this.parentElement.children[this.parentElement.children.indexOf(this) + 1] || null;
  }
  detach() {
    if (!this.parentElement) return;
    const siblings = this.parentElement.children;
    siblings.splice(siblings.indexOf(this), 1);
    this.parentElement = null;
  }
  appendChild(child) { child.detach(); child.parentElement = this; this.children.push(child); return child; }
  insertAdjacentElement(position, child) {
    assert.equal(position, 'afterend');
    assert(this.parentElement);
    const parent = this.parentElement;
    child.detach(); child.parentElement = parent;
    parent.children.splice(parent.children.indexOf(this) + 1, 0, child);
  }
  replaceWith(child) {
    const parent = this.parentElement; assert(parent);
    const index = parent.children.indexOf(this);
    child.detach(); child.parentElement = parent;
    parent.children[index] = child; this.parentElement = null;
  }
  closest(selector) {
    for (let node = this; node; node = node.parentElement) if (node.classList.contains(selector.slice(1))) return node;
    return null;
  }
  querySelector(selector) {
    const wanted = selector.match(/data-artifact-id="([^"]+)"/)?.[1];
    const visit = node => {
      for (const child of node.children) {
        if (child.classList.contains('artifact-chat-card') && child.dataset.artifactId === wanted) return child;
        const found = visit(child); if (found) return found;
      }
      return null;
    };
    return visit(this);
  }
}
let area = new Element('chat-area');
let closed = 0;
const c = {
  CSS: { escape: value => value }, document: { getElementById: id => id === 'chatArea' ? area : null },
  artifactFromPresentation: value => value, upsertArtifact: value => value,
  createArtifactCard: item => new Element('artifact-card artifact-chat-card', item.id),
  activityGroupEl: null,
  finishActivityGroup: () => { if (c.activityGroupEl) closed++; c.activityGroupEl = null; },
  updateEmptyLayout() {}, autoScroll() {}, turnStatusEl: null,
};
vm.createContext(c);
vm.runInContext(source.slice(start, end), c);
const group = () => {
  const g = area.appendChild(new Element('activity-group'));
  const tool = g.appendChild(new Element('call-row'));
  c.activityGroupEl = g;
  return {g, tool};
};
const ids = () => area.children.map(node => node.dataset.artifactId || node.className);

// Live results are visible peers after their process group, in arrival order.
const first = group();
assert.equal(c.renderArtifactPresentation(first.tool, {id:'a'}), true);
assert.equal(closed, 1);
assert.deepEqual(ids(), ['activity-group', 'a']);
assert.equal(area.children[1].parentElement, area);
const siblingTool = first.g.appendChild(new Element('call-row'));
c.renderArtifactPresentation(siblingTool, {id:'b'});
assert.deepEqual(ids(), ['activity-group', 'a', 'b']);
// Updating an existing Artifact preserves its original position and ID uniqueness.
c.renderArtifactPresentation(siblingTool, {id:'a', currentVersion:2});
assert.deepEqual(ids(), ['activity-group', 'a', 'b']);

// A later process section starts after the visible card boundary.
const second = group();
const beforeLate = closed;
c.renderArtifactPresentation(first.tool, {id:'late'});
assert.equal(closed, beforeLate, 'a late result from the old group must not close the current group');
assert.equal(c.activityGroupEl, second.g);
assert.deepEqual(ids(), ['activity-group', 'a', 'b', 'late', 'activity-group']);
c.renderArtifactPresentation(second.tool, {id:'c'});
assert.equal(closed, beforeLate + 1);
assert.deepEqual(ids(), ['activity-group', 'a', 'b', 'late', 'activity-group', 'c']);

// Saved-session replay uses the same renderer; gallery sync repairs legacy
// nested cards instead of treating a hidden card as already visible.
area = new Element('chat-area');
const replay = group();
c.renderArtifactPresentation(replay.tool, {id:'saved'});
const hidden = replay.g.appendChild(new Element('artifact-card artifact-chat-card', 'legacy'));
group();
const beforeSync = closed;
c.syncArtifactChatCards([{id:'legacy'}, {id:'gallery-only'}]);
assert.equal(hidden.parentElement, area);
assert.deepEqual(ids(), ['activity-group', 'legacy', 'saved', 'activity-group', 'gallery-only']);
assert.equal(closed, beforeSync + 1, 'a newly inserted gallery card closes the active process group');
`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("artifact activity placement: %v\n%s", err, output)
	}
}
