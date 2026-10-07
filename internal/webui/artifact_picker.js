'use strict';
// This bridge runs only in the isolated, opaque selection iframe. It never
// reads WebUI state or trusts artifact text to construct its message payload.
const targets = new Map();
document.querySelectorAll('[data-metis-target]').forEach(node => {
  targets.set(node.getAttribute('data-metis-target'), node);
  node.setAttribute('tabindex', '0');
});
let selected = null;
const outline = document.createElement('div');
outline.setAttribute('aria-hidden', 'true');
outline.style.cssText = 'all:initial!important;position:fixed!important;pointer-events:none!important;box-sizing:border-box!important;border:2px solid #5686fe!important;border-radius:5px!important;background:rgba(86,134,254,.10)!important;z-index:2147483647!important;display:none!important;';
document.body.appendChild(outline);
function send(message) {
  window.parent.postMessage(Object.assign({channel: config.channel}, message), config.parentOrigin);
}
function targetAt(node) {
  if (!node || node.nodeType !== 1) return null;
  const target = node.closest('[data-metis-target]');
  return target && targets.get(target.getAttribute('data-metis-target')) === target ? target : null;
}
function paint(node) {
  if (!node) { outline.style.setProperty('display', 'none', 'important'); return; }
  const rect = node.getBoundingClientRect();
  outline.style.setProperty('display', 'block', 'important');
  ['left', 'top', 'width', 'height'].forEach((key, i) => {
    outline.style.setProperty(key, [rect.x, rect.y, rect.width, rect.height][i] + 'px', 'important');
  });
}
document.addEventListener('pointermove', event => { if (!selected) paint(targetAt(event.target)); }, true);
document.addEventListener('pointerleave', () => { if (!selected) paint(null); }, true);
function select(node) {
  if (!node) return;
  selected = node;
  paint(node);
  const rect = node.getBoundingClientRect();
  send({type:'metis-artifact-selection', targetId:node.getAttribute('data-metis-target'), rect:{x:rect.x,y:rect.y,width:rect.width,height:rect.height}});
}
document.addEventListener('click', event => {
  event.preventDefault();
  event.stopImmediatePropagation();
  select(targetAt(event.target));
}, true);
['auxclick', 'dblclick', 'submit'].forEach(type => {
  document.addEventListener(type, event => { event.preventDefault(); event.stopImmediatePropagation(); }, true);
});
document.addEventListener('focusin', event => { if (!selected) paint(targetAt(event.target)); }, true);
document.addEventListener('keydown', event => {
  if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault();
    event.stopImmediatePropagation();
    select(targetAt(event.target));
    return;
  }
  if (event.key !== 'Escape') return;
  event.preventDefault();
  event.stopImmediatePropagation();
  selected = null;
  paint(null);
  send({type:'metis-artifact-annotation-exit'});
}, true);
window.addEventListener('scroll', () => { if (selected) paint(selected); }, true);
window.addEventListener('resize', () => { if (selected) paint(selected); });
document.documentElement.style.setProperty('cursor', 'crosshair', 'important');
send({type:'metis-artifact-annotation-ready'});
