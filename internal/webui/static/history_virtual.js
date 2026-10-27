// History stays as data until a turn enters the viewport. Turn nodes remain
// direct chatArea children so tool, artifact and disclosure ownership agrees
// with the live transcript renderer.

function historyVirtualSignature(entries) {
  const text = JSON.stringify(entries);
  let a = 2166136261, b = 2246822519;
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    a = Math.imul(a ^ code, 16777619);
    b = Math.imul(b ^ code, 3266489917);
  }
  return (a >>> 0).toString(16).padStart(8, '0') + (b >>> 0).toString(16).padStart(8, '0');
}

// Source message indices name persisted messages; visible text indices are
// presentation counters only and must never authorize a fork/retry operation.
function buildHistoryTurnIndex(history, sessionId, options = {}) {
  const visible = options.visibleText || (typeof visibleTranscriptText === 'function' ? visibleTranscriptText : value => String(value || ''));
  const units = [];
  let ordinal = 0, textIndex = 0, current = null;
  (Array.isArray(history) ? history : []).forEach((message, sourceIndex) => {
    const entries = Array.isArray(message && message.content) ? message.content
      : message && message.content != null ? [{ type: 'text', text: String(message.content) }] : [];
    const startsTurn = message && message.role === 'user' && entries.some(block => {
      if (!block || block.type !== 'text') return false;
      const text = String(block.text || '');
      return !text.startsWith('[user steer mid-turn] ') && String(visible(text)).trim();
    });
    if (startsTurn) ordinal++;
    if (!current || startsTurn) {
      current = { key: String(sessionId || '') + ':history-' + ordinal, turn: ordinal, entries: [],
        sourceStart: sourceIndex, sourceEnd: sourceIndex, entrySourceIndices: [],
        textIndexStart: textIndex, textIndexEnd: textIndex, signature: '', estimatedHeight: 1, pinReasons: [] };
      units.push(current);
    }
    current.entries.push(message);
    current.entrySourceIndices.push(sourceIndex);
    current.sourceEnd = sourceIndex + 1;
    entries.forEach(block => {
      if (!block || typeof block !== 'object') return;
      if (block.type === 'text') {
        const raw = String(block.text || '');
        const text = String(visible(raw.startsWith('[user steer mid-turn] ') ? raw.slice(22) : raw));
        if (text.trim()) { textIndex++; current.estimatedHeight += 48 + Math.ceil(text.length / 76) * 20; }
      } else if (block.type === 'thinking' || block.type === 'redacted_thinking') current.estimatedHeight += 40;
      else if (block.type === 'tool_use') current.estimatedHeight += 52;
    });
    current.textIndexEnd = textIndex;
  });
  units.forEach(unit => { unit.signature = historyVirtualSignature(unit.entries); });
  return units;
}

function historyVirtualHeight(unit) {
  const value = unit && (unit.measuredHeight ?? unit.estimatedHeight);
  return Number.isFinite(Number(value)) ? Math.max(1, Number(value)) : 160;
}

// Overscan is measured in pixels. Keep ordinary windows bounded, while a
// viewport containing more than 48 tiny turns still gets complete coverage.
function historyVirtualRange(units, scrollTop, clientHeight, overscan) {
  const count = units.length, offsets = new Array(count + 1);
  offsets[0] = 0;
  for (let i = 0; i < count; i++) offsets[i + 1] = offsets[i] + historyVirtualHeight(units[i]);
  const totalHeight = offsets[count];
  if (!count) return { start: 0, end: 0, totalHeight, offsets };
  const height = Math.max(1, Number(clientHeight) || 1);
  const top = Math.max(0, Math.min(Number(scrollTop) || 0, Math.max(0, totalHeight - height)));
  const extra = overscan == null ? Math.max(400, height) : Math.max(0, Number(overscan) || 0);
  const firstAt = position => {
    let lo = 0, hi = count;
    while (lo < hi) { const mid = (lo + hi) >>> 1; if (offsets[mid + 1] <= position) lo = mid + 1; else hi = mid; }
    return Math.min(count - 1, lo);
  };
  const visibleStart = firstAt(top), visibleEnd = Math.min(count, firstAt(top + height - 0.001) + 1);
  let start = firstAt(Math.max(0, top - extra));
  let end = Math.min(count, firstAt(Math.min(totalHeight - 0.001, top + height + extra)) + 1);
  if (end - start > 48 && visibleEnd - visibleStart <= 48) {
    const spare = 48 - (visibleEnd - visibleStart);
    start = Math.max(start, visibleStart - Math.floor(spare / 2));
    end = Math.min(end, start + 48);
    start = Math.max(0, Math.min(start, end - 48));
  }
  return { start, end, totalHeight, offsets };
}

class HistoryVirtualViewport {
  constructor(options) {
    this.options = options || {};
    this.area = this.options.area;
    this.sessionId = String(this.options.sessionId || '');
    this.units = [];
    this.byKey = new Map();
    this.mounted = new Map();
    this.spacers = new Map();
    this.measurements = new Map();
    this.pins = new Map();
    this.metadata = new Map();
    this.frame = 0;
    this.destroyed = false;
    this.width = 0;
    this.fontFingerprint = '';
    this._restoring = true;
    this._restoreVersion = 0;
    this.lastAnchor = null;
    this.pendingAnchor = this.options.anchor || this._loadAnchor() || { sessionId: this.sessionId, follow: true };
    this.following = this.pendingAnchor.follow !== false;
    if (typeof this.options.onFollow === 'function') this.options.onFollow(this.following);
    this.forcedKey = '';
    this.expectedTop = null;
    this.rendering = false;
    this._scroll = () => {
      if (this.destroyed) return;
      if (this._restoring) return;
      if (this.expectedTop !== null && Math.abs(this.area.scrollTop - this.expectedTop) < 1) { this.expectedTop = null; return; }
      this.expectedTop = null;
      if (typeof this.options.isFollowing !== 'function') this.following = this.area.scrollHeight - this.area.scrollTop - this.area.clientHeight <= 56;
      this.pendingAnchor = this.captureAnchor();
      this.schedule();
    };
    this._reflow = () => {
      if (!this.pendingAnchor && this.lastAnchor) this.pendingAnchor = { ...this.lastAnchor, follow: this._following() };
      this.schedule();
    };
    this._focusChanged = () => this.schedule();
    if (this.area && typeof this.area.addEventListener === 'function') {
      this.area.addEventListener('scroll', this._scroll, { passive: true });
      this.area.addEventListener('load', this._reflow, true);
      this.area.addEventListener('focusin', this._focusChanged);
      this.area.addEventListener('focusout', this._focusChanged);
    }
    if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') document.addEventListener('selectionchange', this._focusChanged);
    if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') window.addEventListener('resize', this._reflow);
    this.observer = typeof ResizeObserver === 'function' ? new ResizeObserver(this._reflow) : null;
    if (this.observer && this.area) this.observer.observe(this.area);
    this.fonts = typeof document !== 'undefined' ? document.fonts : null;
    this._fontLoaded = () => {
      this.measurements.clear();
      this.units.forEach(unit => { delete unit.measuredHeight; });
      this._reflow();
    };
    if (this.fonts && typeof this.fonts.addEventListener === 'function') this.fonts.addEventListener('loadingdone', this._fontLoaded);
    this.setUnits(this.options.units || [], { anchor: this.pendingAnchor });
  }

  _visible() {
    const area = this.area;
    if (!area || !(area.clientWidth > 0) || !(area.clientHeight > 0)) return false;
    return typeof area.getClientRects !== 'function' || area.getClientRects().length > 0;
  }

  _following() { return typeof this.options.isFollowing === 'function' ? !!this.options.isFollowing() : this.following; }

  _storage() {
    try { return typeof sessionStorage !== 'undefined' ? sessionStorage : typeof window !== 'undefined' ? window.sessionStorage : null; }
    catch (_) { return null; }
  }

  _loadAnchor() {
    try {
      const storage = this._storage();
      const saved = storage && JSON.parse(storage.getItem('metis.history-anchors.v1') || 'null');
      return saved && saved.schema === 1 && Array.isArray(saved.items)
        ? saved.items.find(item => item && item.sessionId === this.sessionId) || null : null;
    } catch (_) { return null; }
  }

  _anchorUnit(anchor) {
    if (!anchor || anchor.sessionId && String(anchor.sessionId) !== this.sessionId) return null;
    if (anchor.key && !String(anchor.key).startsWith(this.sessionId + ':history-')) return null;
    const exact = this.byKey.get(String(anchor.key || ''));
    if (exact && anchor.signature === exact.signature) return { unit: exact, offset: Number(anchor.offset) || 0,
      nodeIndex: Number.isInteger(anchor.nodeIndex) && anchor.nodeIndex >= 0 ? anchor.nodeIndex : 0,
      itemIndex: Number.isInteger(anchor.itemIndex) && anchor.itemIndex >= 0 ? anchor.itemIndex : null,
      contentKey: typeof anchor.contentKey === 'string' && /^[a-zA-Z0-9:_-]{1,256}$/.test(anchor.contentKey) ? anchor.contentKey : '',
      withinTurnOffset: Math.max(0, Number(anchor.withinTurnOffset) || 0),
      contentPath: Array.isArray(anchor.contentPath) && anchor.contentPath.length <= 32 && anchor.contentPath.every(index => Number.isInteger(index) && index >= 0)
        ? anchor.contentPath : [] };
    const ordinal = Number(anchor.turn ?? /history-(\d+)$/.exec(String(anchor.key || ''))?.[1]);
    const unit = Number.isFinite(ordinal) ? this.units.find(item => item.turn >= ordinal) || this.units[this.units.length - 1] : this.units[0];
    // Compaction, retry or deletion changed the source. Retain a nearby turn,
    // but never apply a previous generation's pixel offset to new content.
    return unit ? { unit, offset: 0, nodeIndex: 0, itemIndex: null, contentKey: '', contentPath: [], withinTurnOffset: 0 } : null;
  }

  _recordNodes(record) {
    const extra = Array.from(this.area.children || []).filter(node => node.dataset && node.dataset.virtualTurnKey === record.unit.key);
    if (this.observer) {
      for (const node of extra) if (!record.nodes.includes(node)) this.observer.observe(node);
      for (const node of record.nodes) if (node.parentElement !== this.area) this.observer.unobserve(node);
    }
    if (extra.length) record.nodes = extra;
    return record.nodes.filter(node => node && node.parentElement === this.area);
  }

  captureAnchor() {
    if (!this._visible()) return this.pendingAnchor || this.lastAnchor || null;
    const follow = this._following();
    const rect = this.area && this.area.getBoundingClientRect ? this.area.getBoundingClientRect() : { top: 0 };
    for (const unit of this.units) {
      const record = this.mounted.get(unit.key);
      if (!record) continue;
      const nodes = this._recordNodes(record);
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (!first || !last || typeof first.getBoundingClientRect !== 'function') continue;
      if (last.getBoundingClientRect().bottom <= rect.top) continue;
      let nodeIndex = nodes.findIndex(node => node.getBoundingClientRect().bottom > rect.top);
      if (nodeIndex < 0) nodeIndex = 0;
      const peer = nodes[nodeIndex];
      let target = peer;
      const containers = typeof peer.querySelectorAll === 'function'
        ? Array.from(peer.querySelectorAll('.message-content, .think-body, .tc-body, .activity-group-items')) : [];
      const bottom = Number.isFinite(rect.bottom) ? rect.bottom : rect.top + this.area.clientHeight;
      const intersects = node => {
        const bounds = node.getBoundingClientRect();
        return bounds.bottom > rect.top && bounds.top < bottom && bounds.bottom > bounds.top;
      };
      const container = containers.find(node => intersects(node));
      if (container) {
        target = container;
        // Retain a block inside a long assistant reply, rather than its user
        // message far above the viewport. Earlier image/code expansion can
        // move this block without moving the turn's first peer at all.
        for (let depth = 0; depth < 24; depth++) {
          const children = Array.from(target.children || []).filter(node =>
            /^(?:P|DIV|PRE|BLOCKQUOTE|UL|OL|LI|TABLE|THEAD|TBODY|TR|TH|TD|H[1-6]|IMG)$/i.test(String(node.tagName || '')) && intersects(node));
          const child = children.find(node => node.getBoundingClientRect().top >= rect.top) || children[0];
          if (!child) break;
          target = child;
        }
      }
      let owner = peer, contentKey = '';
      for (let node = target; node; node = node.parentElement) {
        if (node.dataset && /^[a-zA-Z0-9:_-]{1,256}$/.test(String(node.dataset.virtualContentKey || ''))) {
          owner = node;
          contentKey = String(node.dataset.virtualContentKey);
          break;
        }
        if (node === peer) break;
      }
      const contentPath = [];
      for (let node = target; node && node !== owner; node = node.parentElement) {
        const parent = node.parentElement;
        if (!parent) { contentPath.length = 0; target = peer; break; }
        contentPath.unshift(Array.from(parent.children || []).indexOf(node));
      }
      return { sessionId: this.sessionId, key: unit.key, turn: unit.turn, signature: unit.signature,
        nodeIndex, itemIndex: peer.dataset && /^\d+$/.test(String(peer.dataset.virtualItemKey)) ? Number(peer.dataset.virtualItemKey) : null,
        contentKey, contentPath, withinTurnOffset: Math.max(0, target.getBoundingClientRect().top - first.getBoundingClientRect().top),
        offset: target.getBoundingClientRect().top - rect.top, follow };
    }
    const unit = this.units[follow ? this.units.length - 1 : 0];
    return unit ? { sessionId: this.sessionId, key: unit.key, turn: unit.turn, signature: unit.signature, offset: 0, follow } : null;
  }

  restoreAnchor(anchor) {
    if (this.destroyed || !anchor || anchor.sessionId && String(anchor.sessionId) !== this.sessionId) return false;
    if (anchor.key && !String(anchor.key).startsWith(this.sessionId + ':history-')) return false;
    const resolved = this._anchorUnit(anchor);
    if (!resolved && this.units.length) return false;
    this._restoring = true;
    this._restoreVersion++;
    this.pendingAnchor = resolved ? { sessionId: this.sessionId, key: resolved.unit.key, turn: resolved.unit.turn,
      signature: resolved.unit.signature, nodeIndex: resolved.nodeIndex, itemIndex: resolved.itemIndex, contentKey: resolved.contentKey, contentPath: resolved.contentPath,
      withinTurnOffset: resolved.withinTurnOffset, offset: resolved.offset, follow: !!anchor.follow } : { sessionId: this.sessionId, follow: !!anchor.follow };
    this.forcedKey = resolved && !anchor.follow ? resolved.unit.key : '';
    this.following = !!anchor.follow;
    if (typeof this.options.onFollow === 'function') this.options.onFollow(!!anchor.follow);
    this.schedule();
    return true;
  }

  setUnits(units, options = {}) {
    if (this.destroyed) return;
    if (options.anchor) { this._restoring = true; this._restoreVersion++; }
    const anchor = options.anchor || this.pendingAnchor || this.captureAnchor();
    const next = Array.isArray(units) ? units : [];
    const previousByKey = this.byKey;
    const nextByKey = new Map(next.map(unit => [unit.key, unit]));
    for (const [key, record] of this.mounted) {
      const replacement = nextByKey.get(key);
      if (!replacement || replacement.signature !== record.unit.signature) this._unmount(key);
      else record.unit = replacement;
    }
    this.units = next;
    this.byKey = nextByKey;
    const invalidated = key => !this.byKey.has(key) || previousByKey.get(key)?.signature !== this.byKey.get(key).signature;
    for (const key of this.pins.keys()) if (invalidated(key)) this.pins.delete(key);
    for (const key of this.metadata.keys()) if (invalidated(key)) this.metadata.delete(key);
    this.pendingAnchor = anchor;
    this.schedule();
  }

  getUnit(turnOrKey) { return typeof turnOrKey === 'number' ? this.units.find(unit => unit.turn === turnOrKey) || null : this.byKey.get(String(turnOrKey)) || null; }

  setPinned(key, reason, on = true) {
    if (this.destroyed || !this.byKey.has(key)) return;
    if (!this.pins.has(key)) { if (!on) return; this.pins.set(key, new Set()); }
    const reasons = this.pins.get(key);
    const name = String(reason || 'active');
    if (reasons.has(name) === !!on) return;
    if (on) reasons.add(name); else reasons.delete(name);
    if (!reasons.size) this.pins.delete(key);
    this.schedule();
  }

  updateMetadata(key, value) {
    if (!this.byKey.has(key)) return;
    this.metadata.set(key, value);
    const record = this.mounted.get(key);
    if (record && typeof this.options.updateMetadata === 'function') this.options.updateMetadata(record.unit, record.nodes, value);
    this.schedule();
  }

  adoptTailNodes(nodes) {
    if (this.destroyed || this.rendering || !this.units.length) return;
    const unit = this.units[this.units.length - 1];
    const record = this.mounted.get(unit.key);
    const candidates = nodes ? Array.from(nodes) : Array.from(this.area.children || []);
    for (const node of candidates) {
      if (!node || node.parentElement !== this.area || !node.dataset || node.dataset.virtualTurnKey || node.dataset.historyVirtualSpacer !== undefined) continue;
      node.dataset.virtualTurnKey = unit.key;
      if (record && !record.nodes.includes(node)) { record.nodes.push(node); record.adopted = true; }
      if (this.observer) this.observer.observe(node);
    }
  }

  reveal(key) {
    const unit = this.getUnit(key);
    if (!unit) return false;
    return this.restoreAnchor({ sessionId: this.sessionId, key: unit.key, turn: unit.turn, signature: unit.signature, offset: 0, follow: false });
  }

  // Input ownership wins over a ResizeObserver or metadata update queued
  // earlier in the same frame. The host calls this after pausing its follow
  // state; the next update captures the user's latest physical position.
  userScrollIntent() {
    if (this.destroyed) return;
    this._restoring = false;
    this._restoreVersion++;
    this.pendingAnchor = null;
    this.lastAnchor = null;
    this.forcedKey = '';
    this.expectedTop = null;
    this.following = false;
    this.schedule();
  }

  schedule() {
    if (this.destroyed || this.frame) return;
    if (typeof requestAnimationFrame === 'function') this.frame = requestAnimationFrame(() => { this.frame = 0; this._update(); });
    else this._update();
  }

  flush() {
    if (this.destroyed || this.rendering || !this._visible()) return false;
    if (this.frame && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(this.frame);
    this.frame = 0;
    this._update();
    return true;
  }

  _unmount(key) {
    const record = this.mounted.get(key);
    if (!record) return;
    const nodes = this._recordNodes(record);
    if (typeof this.options.beforeUnmount === 'function') this.options.beforeUnmount(record.unit, nodes);
    for (const node of nodes) { if (this.observer) this.observer.unobserve(node); node.remove(); }
    this.mounted.delete(key);
  }

  _measure() {
    const width = Math.max(1, Math.round(this.area.clientWidth));
    const style = typeof getComputedStyle === 'function' ? getComputedStyle(this.area) : null;
    const font = style ? [style.fontFamily, style.fontSize, style.lineHeight, style.letterSpacing].join('|') : '';
    if (this.fontFingerprint && this.fontFingerprint !== font) {
      this.measurements.clear();
      this.units.forEach(unit => { delete unit.measuredHeight; });
    }
    this.fontFingerprint = font;
    this.width = width;
    let changed = false;
    for (const unit of this.units) {
      const cached = this.measurements.get(unit.key + '|' + unit.signature + '|' + width);
      if (cached !== undefined) unit.measuredHeight = cached;
      else delete unit.measuredHeight;
    }
    for (const record of this.mounted.values()) {
      const nodes = this._recordNodes(record);
      if (!nodes.length) continue;
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (typeof first.getBoundingClientRect !== 'function') continue;
      const top = first.getBoundingClientRect().top, bottom = last.getBoundingClientRect().bottom;
      const firstStyle = typeof getComputedStyle === 'function' ? getComputedStyle(first) : null;
      const lastStyle = typeof getComputedStyle === 'function' ? getComputedStyle(last) : null;
      const height = Math.max(1, bottom - top + (parseFloat(firstStyle && firstStyle.marginTop) || 0) + (parseFloat(lastStyle && lastStyle.marginBottom) || 0));
      if (!Number.isFinite(height)) continue;
      if (Math.abs(historyVirtualHeight(record.unit) - height) > 0.5) changed = true;
      record.unit.measuredHeight = height;
      const key = record.unit.key + '|' + record.unit.signature + '|' + width;
      this.measurements.delete(key);
      this.measurements.set(key, height);
    }
    while (this.measurements.size > 4096) this.measurements.delete(this.measurements.keys().next().value);
    return changed;
  }

  _wanted(range) {
    const wanted = new Set();
    const endpoints = this._endpointPins();
    for (let index = range.start; index < range.end; index++) wanted.add(index);
    this.units.forEach((unit, index) => {
      if (index === this.units.length - 1 || unit.key === this.forcedKey ||
          unit.pinReasons && unit.pinReasons.length || this.pins.has(unit.key) || endpoints.has(unit.key)) wanted.add(index);
    });
    return [...wanted].sort((a, b) => a - b);
  }

  _endpointPins() {
    const keys = new Set();
    const add = target => {
      let key = '';
      for (let node = target; node; node = node.parentElement || node.parentNode) {
        if (!key && node.dataset && node.dataset.virtualTurnKey) key = node.dataset.virtualTurnKey;
        if (node === this.area) { if (this.byKey.has(key)) keys.add(key); break; }
      }
    };
    if (typeof document !== 'undefined') add(document.activeElement);
    const selection = typeof window !== 'undefined' && typeof window.getSelection === 'function' ? window.getSelection()
      : typeof document !== 'undefined' && typeof document.getSelection === 'function' ? document.getSelection() : null;
    if (selection) { add(selection.anchorNode); add(selection.focusNode); }
    return keys;
  }

  _captureDOMInteraction() {
    const selection = typeof window !== 'undefined' && typeof window.getSelection === 'function' ? window.getSelection()
      : typeof document !== 'undefined' && typeof document.getSelection === 'function' ? document.getSelection() : null;
    // Range objects are live: moving a peer can mutate their endpoints. Keep
    // raw node references and numeric offsets before touching any DOM instead.
    return { active: typeof document !== 'undefined' ? document.activeElement : null,
      anchorNode: selection && selection.anchorNode, anchorOffset: selection && selection.anchorOffset,
      focusNode: selection && selection.focusNode, focusOffset: selection && selection.focusOffset };
  }

  _restoreDOMInteraction(saved) {
    if (!saved) return;
    const active = saved.active;
    if (active && active.isConnected && typeof active.focus === 'function' && typeof document !== 'undefined' &&
        document.activeElement !== active && typeof this.area.contains === 'function' && this.area.contains(active)) {
      try { active.focus({ preventScroll: true }); } catch (_) { /* A removed/native control can reject focus restoration. */ }
    }
    if (!saved.anchorNode || !saved.focusNode || !saved.anchorNode.isConnected || !saved.focusNode.isConnected) return;
    const selection = typeof window !== 'undefined' && typeof window.getSelection === 'function' ? window.getSelection()
      : typeof document !== 'undefined' && typeof document.getSelection === 'function' ? document.getSelection() : null;
    if (!selection || selection.anchorNode === saved.anchorNode && selection.anchorOffset === saved.anchorOffset &&
        selection.focusNode === saved.focusNode && selection.focusOffset === saved.focusOffset) return;
    try {
      if (typeof selection.setBaseAndExtent === 'function') {
        selection.setBaseAndExtent(saved.anchorNode, saved.anchorOffset, saved.focusNode, saved.focusOffset);
      } else if (typeof selection.collapse === 'function' && typeof selection.extend === 'function') {
        selection.collapse(saved.anchorNode, saved.anchorOffset);
        selection.extend(saved.focusNode, saved.focusOffset);
      } else if (typeof document !== 'undefined' && typeof document.createRange === 'function') {
        const range = document.createRange();
        range.setStart(saved.anchorNode, saved.anchorOffset);
        range.setEnd(saved.focusNode, saved.focusOffset);
        if (range.collapsed && (saved.anchorNode !== saved.focusNode || saved.anchorOffset !== saved.focusOffset)) {
          range.setStart(saved.focusNode, saved.focusOffset);
          range.setEnd(saved.anchorNode, saved.anchorOffset);
        }
        selection.removeAllRanges();
        selection.addRange(range);
      }
    } catch (_) { /* Content replacement may invalidate an endpoint after its owning source changes. */ }
  }

  _syncDOM(indices, offsets) {
    const interaction = this._captureDOMInteraction();
    try {
      const desired = [], usedSpacers = new Set();
      let previous = 0;
      const gap = (start, end) => {
        if (end <= start) return;
        const key = String(start) + ':' + end;
        let spacer = this.spacers.get(key);
        if (!spacer) {
          spacer = this.area.ownerDocument ? this.area.ownerDocument.createElement('div') : document.createElement('div');
          spacer.className = 'history-virtual-spacer';
          spacer.dataset.historyVirtualSpacer = key;
          spacer.setAttribute('aria-hidden', 'true');
          this.spacers.set(key, spacer);
        }
        const height = Math.max(0, offsets[end] - offsets[start]);
        const value = height + 'px';
        if (spacer.style.height !== value) spacer.style.height = value;
        desired.push(spacer);
        usedSpacers.add(key);
      };
      for (const index of indices) {
        gap(previous, index);
        const record = this.mounted.get(this.units[index].key);
        if (record) desired.push(...this._recordNodes(record));
        previous = index + 1;
      }
      gap(previous, this.units.length);
      for (const [key, spacer] of this.spacers) if (!usedSpacers.has(key)) { spacer.remove(); this.spacers.delete(key); }
      let cursor = this.area.firstChild || this.area.firstElementChild || (this.area.children || [])[0] || null;
      for (const node of desired) {
        if (node === cursor) { cursor = cursor.nextSibling || null; continue; }
        this.area.insertBefore(node, cursor);
      }
    } finally {
      this._restoreDOMInteraction(interaction);
    }
  }

  // Active history needs its existing tool rows before the next SSE event,
  // including the interval before the first virtual-window animation frame.
  ensureMounted(key) {
    if (this.destroyed) return null;
    const unit = this.getUnit(key);
    if (!unit) return null;
    if (this.mounted.has(unit.key)) return this.mounted.get(unit.key);
    const wasRendering = this.rendering;
    this.rendering = true;
    try {
      const nodes = Array.from(this.options.renderTurn(unit) || []);
      for (const node of nodes) {
        if (node.dataset) node.dataset.virtualTurnKey = unit.key;
        if (this.observer) this.observer.observe(node);
      }
      const existing = Array.from(this.area.children || []).filter(node => node.dataset && node.dataset.virtualTurnKey === unit.key && !nodes.includes(node));
      const record = { unit, nodes: nodes.concat(existing), adopted: existing.length > 0 };
      this.mounted.set(unit.key, record);
      if (typeof this.options.afterMount === 'function') this.options.afterMount(unit, record.nodes);
      if (this.metadata.has(unit.key) && typeof this.options.updateMetadata === 'function') this.options.updateMetadata(unit, record.nodes, this.metadata.get(unit.key));
      return record;
    } finally {
      this.rendering = wasRendering;
    }
  }

  _setScroll(top) {
    if (this.area._metisScrollGesture?.active) return;
    this.area.scrollTop = Math.max(0, top);
    this.expectedTop = this.area.scrollTop;
  }

  _applyAnchor(anchor, range) {
    if (!anchor || this.area._metisScrollGesture?.active) return;
    if (anchor.follow) { this._setScroll(this.area.scrollHeight); return; }
    const resolved = this._anchorUnit(anchor);
    if (!resolved) return;
    const record = this.mounted.get(resolved.unit.key);
    const nodes = record && this._recordNodes(record);
    let target = null, desiredOffset = resolved.offset, path = resolved.contentPath || [];
    if (nodes && resolved.contentKey) {
      for (const peer of nodes) {
        if (peer.dataset && peer.dataset.virtualContentKey === resolved.contentKey) { target = peer; break; }
        if (typeof peer.querySelectorAll === 'function') target = Array.from(peer.querySelectorAll('[data-virtual-content-key]')).find(node => node.dataset.virtualContentKey === resolved.contentKey);
        if (target) break;
      }
      // A transient row disappeared in the canonical snapshot. Do not apply
      // its old offset to an unrelated peer which happens to share its index.
      if (!target) { target = nodes[0]; desiredOffset = 0; path = []; }
    } else {
      target = nodes && (resolved.itemIndex !== null ? nodes.find(node => node.dataset && node.dataset.virtualItemKey === String(resolved.itemIndex)) : null);
      if (!target) target = nodes && nodes[resolved.nodeIndex || 0];
    }
    for (const index of path) {
      if (!target || !target.children || !target.children[index]) { target = nodes && nodes[0]; desiredOffset = 0; break; }
      target = target.children[index];
    }
    if (target && typeof target.getBoundingClientRect === 'function') {
      const delta = target.getBoundingClientRect().top - this.area.getBoundingClientRect().top - desiredOffset;
      if (Math.abs(delta) > 0.5) this._setScroll(this.area.scrollTop + delta);
    } else {
      const index = this.units.indexOf(resolved.unit);
      this._setScroll(range.offsets[index] + resolved.withinTurnOffset - resolved.offset);
    }
  }

  _update() {
    if (this.destroyed || !this._visible()) return;
    const restoreVersion = this._restoreVersion;
    let anchor = this.pendingAnchor || this.captureAnchor();
    if (anchor && typeof this.options.isFollowing === 'function') anchor = { ...anchor, follow: !!this.options.isFollowing() };
    this.pendingAnchor = null;
    this._measure();
    let range = historyVirtualRange(this.units, this.area.scrollTop, this.area.clientHeight, this.options.overscan);
    let top = this.area.scrollTop;
    if (anchor && anchor.follow) top = Math.max(0, range.totalHeight - this.area.clientHeight);
    else if (anchor) {
      const resolved = this._anchorUnit(anchor);
      if (resolved) {
        top = range.offsets[this.units.indexOf(resolved.unit)] + resolved.withinTurnOffset - resolved.offset;
        this.forcedKey = resolved.unit.key;
      }
    }
    range = historyVirtualRange(this.units, top, this.area.clientHeight, this.options.overscan);
    const wanted = this._wanted(range), keys = new Set(wanted.map(index => this.units[index].key));
    for (const key of this.mounted.keys()) if (!keys.has(key)) this._unmount(key);
    for (const index of wanted) {
      const unit = this.units[index];
      this.ensureMounted(unit.key);
    }
    this._syncDOM(wanted, range.offsets);
    const changed = this._measure();
    if (changed) { range = historyVirtualRange(this.units, top, this.area.clientHeight, this.options.overscan); this._syncDOM(wanted, range.offsets); }
    if (restoreVersion !== this._restoreVersion) { this.schedule(); return; }
    if (anchor) this.following = !!anchor.follow;
    this._applyAnchor(anchor, range);
    this._restoring = false;
    if (this.forcedKey && range.start <= this.units.indexOf(this.byKey.get(this.forcedKey)) && this.units.indexOf(this.byKey.get(this.forcedKey)) < range.end) this.forcedKey = '';
    this.lastAnchor = this.captureAnchor();
    if (typeof this.options.onViewport === 'function') this.options.onViewport(this.lastAnchor);
    if (restoreVersion !== this._restoreVersion) return;
    const actualRange = historyVirtualRange(this.units, this.area.scrollTop, this.area.clientHeight, this.options.overscan);
    const actualKeys = this._wanted(actualRange).map(index => this.units[index].key);
    if (actualKeys.length !== this.mounted.size || actualKeys.some(key => !this.mounted.has(key))) {
      this.pendingAnchor = this.lastAnchor;
      this.schedule();
    }
  }

  stats() {
    let pinned = 0;
    const endpoints = this._endpointPins();
    this.units.forEach((unit, index) => { if (index === this.units.length - 1 || unit.pinReasons && unit.pinReasons.length || this.pins.has(unit.key) || endpoints.has(unit.key)) pinned++; });
    return { total: this.units.length, mounted: this.mounted.size, pinned };
  }

  save() {
    const anchor = this.pendingAnchor || this.captureAnchor() || this.lastAnchor;
    if (!anchor) return null;
    const item = { sessionId: this.sessionId.slice(0, 200), key: String(anchor.key || '').slice(0, 512),
      turn: Number(anchor.turn) || 0, signature: String(anchor.signature || '').slice(0, 64),
      nodeIndex: Number.isInteger(anchor.nodeIndex) && anchor.nodeIndex >= 0 ? anchor.nodeIndex : 0,
      itemIndex: Number.isInteger(anchor.itemIndex) && anchor.itemIndex >= 0 ? anchor.itemIndex : null,
      contentKey: typeof anchor.contentKey === 'string' && /^[a-zA-Z0-9:_-]{1,256}$/.test(anchor.contentKey) ? anchor.contentKey : '',
      contentPath: Array.isArray(anchor.contentPath) ? anchor.contentPath.filter(index => Number.isInteger(index) && index >= 0).slice(0, 32) : [],
      withinTurnOffset: Math.max(0, Math.min(10000000, Number(anchor.withinTurnOffset) || 0)),
      offset: Math.max(-10000000, Math.min(10000000, Number(anchor.offset) || 0)), follow: !!anchor.follow };
    try {
      const storage = this._storage();
      if (storage) {
        let saved;
        try { saved = JSON.parse(storage.getItem('metis.history-anchors.v1') || 'null'); } catch (_) { saved = null; }
        const items = saved && saved.schema === 1 && Array.isArray(saved.items) ? saved.items.filter(value => value && value.sessionId !== this.sessionId) : [];
        storage.setItem('metis.history-anchors.v1', JSON.stringify({ schema: 1, items: [item, ...items].slice(0, 32) }));
      }
    } catch (_) { /* Sandboxed storage can be unavailable; the in-memory anchor remains usable. */ }
    return item;
  }

  destroy() {
    if (this.destroyed) return;
    this.save();
    this.destroyed = true;
    if (this.frame && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(this.frame);
    this.frame = 0;
    if (this.observer) this.observer.disconnect();
    if (this.fonts && typeof this.fonts.removeEventListener === 'function') this.fonts.removeEventListener('loadingdone', this._fontLoaded);
    if (this.area && typeof this.area.removeEventListener === 'function') {
      this.area.removeEventListener('scroll', this._scroll);
      this.area.removeEventListener('load', this._reflow, true);
      this.area.removeEventListener('focusin', this._focusChanged);
      this.area.removeEventListener('focusout', this._focusChanged);
    }
    if (typeof document !== 'undefined' && typeof document.removeEventListener === 'function') document.removeEventListener('selectionchange', this._focusChanged);
    if (typeof window !== 'undefined' && typeof window.removeEventListener === 'function') window.removeEventListener('resize', this._reflow);
    for (const key of this.mounted.keys()) this._unmount(key);
    for (const spacer of this.spacers.values()) spacer.remove();
    this.spacers.clear();
    this.metadata.clear();
    this.measurements.clear();
    this.pins.clear();
  }
}
