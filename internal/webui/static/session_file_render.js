// Offline, read-only source rendering. Prism is only used as a tokenizer:
// every source fragment is escaped here, and no Prism HTML/hooks are executed.
const SESSION_FILE_SOURCE_MAX_CHARS = 512 * 1024;
const SESSION_FILE_SOURCE_MAX_LINES = 12000;
const SESSION_FILE_HIGHLIGHT_MAX_CHARS = 96 * 1024;
const SESSION_FILE_HIGHLIGHT_MAX_LINE = 2048;

function sessionFileLanguage(file) {
  const path = String(typeof file === 'string' ? file : file?.name || file?.path || '').replace(/\\/g, '/');
  const name = path.slice(path.lastIndexOf('/') + 1).toLowerCase();
  let id = 'plaintext', label = 'Plain Text';
  const extensions = {
    md: ['markdown', 'Markdown'], markdown: ['markdown', 'Markdown'], mdown: ['markdown', 'Markdown'],
    js: ['javascript', 'JavaScript'], mjs: ['javascript', 'JavaScript'], cjs: ['javascript', 'JavaScript'], jsx: ['javascript', 'JavaScript JSX'],
    ts: ['typescript', 'TypeScript'], mts: ['typescript', 'TypeScript'], cts: ['typescript', 'TypeScript'], tsx: ['typescript', 'TypeScript JSX'],
    sh: ['bash', 'Shell'], bash: ['bash', 'Shell'], zsh: ['bash', 'Shell'], ksh: ['bash', 'Shell'],
    go: ['go', 'Go'], py: ['python', 'Python'], pyw: ['python', 'Python'], rs: ['rust', 'Rust'],
    json: ['json', 'JSON'], jsonl: ['json', 'JSON Lines'], jsonc: ['javascript', 'JSON with Comments'],
    yaml: ['yaml', 'YAML'], yml: ['yaml', 'YAML'], toml: ['toml', 'TOML'], ini: ['ini', 'INI'], cfg: ['ini', 'INI'],
    html: ['markup', 'HTML'], htm: ['markup', 'HTML'], xml: ['markup', 'XML'], svg: ['markup', 'SVG'], vue: ['markup', 'Vue'],
    css: ['css', 'CSS'], java: ['java', 'Java'], c: ['c', 'C'], h: ['c', 'C'], cpp: ['cpp', 'C++'], cc: ['cpp', 'C++'], cxx: ['cpp', 'C++'], hpp: ['cpp', 'C++'], cs: ['csharp', 'C#'],
    sql: ['sql', 'SQL'], diff: ['diff', 'Diff'], patch: ['diff', 'Diff'], csv: ['plaintext', 'CSV'], tsv: ['plaintext', 'TSV'], log: ['plaintext', 'Log'],
  };
  const extension = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1) : '';
  if (Object.prototype.hasOwnProperty.call(extensions, extension)) [id, label] = extensions[extension];
  else if (/^(?:dockerfile|containerfile)(?:\.|$)/.test(name)) [id, label] = ['docker', 'Dockerfile'];
  else if (/^(?:\.?(?:bashrc|bash_profile|bash_login|zshrc|zprofile|profile)|\.env(?:\..*)?)$/.test(name)) [id, label] = ['bash', 'Shell'];
  else if (name === 'cargo.lock' || name === 'poetry.lock' || name === 'uv.lock') [id, label] = ['toml', 'TOML'];
  else if (name === 'go.mod' || name === 'go.sum') [id, label] = ['go', 'Go'];
  else if (typeof file === 'object' && file?.kind === 'markdown' && !extension) [id, label] = ['markdown', 'Markdown'];
  return { id, label };
}

function sessionFileSourceModel(content) {
  const original = String(content ?? '');
  let text = original.slice(0, SESSION_FILE_SOURCE_MAX_CHARS);
  // A byte-bounded backend normally makes this guard unnecessary. Keep direct
  // callers bounded too, without cutting a UTF-16 surrogate pair in half.
  if (text.length < original.length && /[\uD800-\uDBFF]$/.test(text)) text = text.slice(0, -1);
  let limited = text.length < original.length;
  const lines = text.replace(/\r\n?/g, '\n').split('\n');
  if (lines.length > SESSION_FILE_SOURCE_MAX_LINES) {
    lines.length = SESSION_FILE_SOURCE_MAX_LINES;
    limited = true;
  }
  return { lines, text: lines.join('\n'), limited };
}

function sessionFileEscapeSource(value) {
  return String(value).replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]);
}

function sessionFileTokenLines(tokens) {
  const lines = [''];
  let visited = 0;
  function visit(token, classes, depth) {
    if (++visited > 100000 || depth > 80) throw new Error('Source token limit');
    if (typeof token === 'string') {
      const parts = token.split('\n');
      parts.forEach((part, index) => {
        if (index) lines.push('');
        if (!part) return;
        const safe = sessionFileEscapeSource(part);
        lines[lines.length - 1] += classes.length ? '<span class="token ' + classes.join(' ') + '">' + safe + '</span>' : safe;
      });
      return;
    }
    if (Array.isArray(token)) {
      token.forEach(child => visit(child, classes, depth + 1));
      return;
    }
    if (!token || typeof token !== 'object') throw new Error('Invalid source token');
    // Grammar classes are metadata, never source HTML. Restrict even these to
    // simple names so accidental custom grammars cannot introduce attributes.
    const aliases = Array.isArray(token.alias) ? token.alias : [token.alias];
    const own = [token.type, ...aliases].filter(value => typeof value === 'string' && /^[a-z][a-z\d-]{0,63}$/i.test(value));
    visit(token.content, [...new Set([...classes, ...own])], depth + 1);
  }
  visit(tokens, [], 0);
  return lines;
}

function sessionFileHighlightedLines(content, file) {
  const model = sessionFileSourceModel(content);
  const plain = () => model.lines.map(sessionFileEscapeSource);
  const language = sessionFileLanguage(file);
  if (language.id === 'plaintext' || !model.text || model.text.length > SESSION_FILE_HIGHLIGHT_MAX_CHARS ||
      model.lines.some(line => line.length > SESSION_FILE_HIGHLIGHT_MAX_LINE) ||
      typeof Prism === 'undefined' || typeof Prism.tokenize !== 'function') return plain();
  const grammar = Object.prototype.hasOwnProperty.call(Prism.languages || {}, language.id) ? Prism.languages[language.id] : null;
  if (!grammar) return plain();
  try {
    const lines = sessionFileTokenLines(Prism.tokenize(model.text, grammar));
    return lines.length === model.lines.length ? lines : plain();
  } catch (_) {
    // Unsupported, excessive, or malformed tokenization cannot block a file.
    return plain();
  }
}

function renderSessionFileSource(content, file, options = {}) {
  const model = sessionFileSourceModel(content);
  const language = sessionFileLanguage(file);
  const highlighted = sessionFileHighlightedLines(model.text, file);
  const source = document.createElement('div');
  source.className = 'session-file-code' + (options.wrap ? ' is-wrapped' : '');
  source.dataset.language = language.id;
  source.dataset.limited = String(model.limited);
  source.tabIndex = 0;
  source.setAttribute('dir', 'ltr');
  source.setAttribute('aria-label', typeof uiText === 'function' ? uiText('Source code, read only', '源代码，只读') : 'Source code, read only');
  highlighted.forEach((html, index) => {
    const row = document.createElement('div');
    row.className = 'session-file-code-line';
    const number = document.createElement('span');
    number.className = 'session-file-line-number';
    number.textContent = String(index + 1);
    number.setAttribute('aria-hidden', 'true');
    const text = document.createElement('code');
    text.className = 'session-file-line-text';
    // html consists only of escaped text and our fixed span/class vocabulary.
    text.innerHTML = html;
    row.append(number, text);
    source.append(row);
  });
  if (model.limited) {
    const notice = document.createElement('div');
    notice.className = 'session-file-source-limit';
    notice.setAttribute('role', 'note');
    notice.textContent = typeof uiText === 'function'
      ? uiText('Only the first 12,000 lines or 512 KiB are displayed. Open the file to view the full content.', '仅显示前 12,000 行或 512 KiB，打开文件可查看完整内容。')
      : 'Only the first 12,000 lines or 512 KiB are displayed. Open the file to view the full content.';
    source.append(notice);
  }
  return source;
}
