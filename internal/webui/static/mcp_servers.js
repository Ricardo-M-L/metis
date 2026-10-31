// This is a read-only snapshot. Listing servers must never start them.
const mcpServersState = { data: null, loading: false, requested: false, error: false, generation: 0, loadedAt: 0 };

function mcpServersActive() {
  const overlay = document.getElementById('settingsOverlay');
  return !!overlay && !overlay.hidden && settingsTab === 'mcp';
}
function mcpServersText(en, zh) { return uiText(en, zh); }
function mcpServerStatus(server) {
  if (server.enabled === false || server.status === 'disabled') return { label: mcpServersText('Disabled', '已禁用'), kind: 'disabled' };
  switch (server.status) {
    case 'registered': return { label: mcpServersText('Registered', '已注册'), kind: 'registered' };
    case 'lazy': return { label: mcpServersText('Connects when used', '使用时连接'), kind: 'idle' };
    case 'stopped': return { label: mcpServersText('Not connected', '未连接'), kind: 'idle' };
    case 'error': case 'failed': return { label: mcpServersText('Connection failed', '连接失败'), kind: 'error' };
    default: return { label: mcpServersText('Status unknown', '状态未知'), kind: 'unknown' };
  }
}
function mcpServerSource(source) {
  switch (source) {
    case 'user': return mcpServersText('User configuration', '用户配置');
    case 'config': return mcpServersText('Configuration', '配置文件');
    case 'plugin': return mcpServersText('Plugin', '插件');
    case 'runtime': return mcpServersText('Runtime', '运行时');
    default: return mcpServersText('Unknown source', '来源未知');
  }
}
function mcpServerTransport(transport) {
  switch (transport) {
    case 'stdio': return mcpServersText('Configured transport: stdio', '配置传输：stdio');
    case 'http': return mcpServersText('Configured transport: HTTP', '配置传输：HTTP');
    default: return mcpServersText('Configured transport: unknown', '配置传输未知');
  }
}
function mcpServersMarkup(data) {
  const servers = Array.isArray(data.servers) ? data.servers : [];
  const count = new Intl.NumberFormat(document.documentElement.lang === 'zh-CN' ? 'zh-CN' : 'en-US').format(servers.length);
  const cards = servers.map(server => {
    const status = mcpServerStatus(server), name = String(server.name || mcpServersText('Unnamed server', '未命名服务器'));
    // A registered client is not an active liveness probe or guarantee.
    const tools = server.status === 'registered' && Number.isSafeInteger(server.toolCount) && server.toolCount >= 0
      ? `<span>${escHtml(mcpServersText(`${server.toolCount} tools discovered`, `发现 ${server.toolCount} 个工具`))}</span>` : '';
    const retained = server.enabled === false && (server.status === 'registered' || server.status === 'lazy')
      ? `<p class="mcp-server-note">${mcpServersText('The current backend still holds a previous client or lazy handle; restart to apply the disabled configuration.', '当前后台仍保留旧客户端或延迟句柄；重启后按禁用配置生效。')}</p>` : '';
    return `<li class="mcp-server-card"><div class="mcp-server-mark" aria-hidden="true"><svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="3" width="14" height="5" rx="1.5"/><rect x="3" y="12" width="14" height="5" rx="1.5"/><path d="M6 5.5h.01M6 14.5h.01M10 8v4"/></svg></div><div class="mcp-server-details"><div class="mcp-server-top"><h3>${escHtml(name)}</h3><span class="mcp-status mcp-status--${status.kind}"><span aria-hidden="true"></span>${escHtml(status.label)}</span></div><div class="mcp-server-meta"><span>${escHtml(mcpServerSource(server.source))}</span><span>${escHtml(mcpServerTransport(server.transport))}</span>${tools}</div>${retained}</div></li>`;
  }).join('');
  const content = servers.length
    ? `<ul class="mcp-server-list" aria-label="${escAttr(mcpServersText('MCP servers', 'MCP 服务器列表'))}">${cards}</ul>`
    : `<div class="mcp-server-empty"><strong>${mcpServersText('No MCP servers configured', '尚未配置 MCP 服务器')}</strong><p>${mcpServersText('Configured servers will appear here.', '配置的服务器会显示在这里。')}</p></div>`;
  const incomplete = data.pluginScanIncomplete === true
    ? `<p class="mcp-servers-incomplete" role="status">${mcpServersText('Some plugin manifests could not be read or verified; check the Plugins page.', '部分插件清单无法读取或验证，请到插件页检查。')}</p>` : '';
  return `<div class="mcp-servers-page"><header class="mcp-servers-heading"><div><h2>${mcpServersText('MCP servers', 'MCP 服务器')}</h2><p>${mcpServersText('Configured servers and handles in this Desktop backend', '配置的服务器及当前 Desktop 后台的句柄')}</p></div><button type="button" class="mcp-servers-refresh" onclick="refreshMcpServers()" ${mcpServersState.loading ? 'disabled' : ''}>${mcpServersState.loading ? mcpServersText('Refreshing…', '正在刷新…') : mcpServersText('Refresh', '刷新')}</button></header><div class="mcp-servers-count"><strong>${mcpServersText('Servers', '服务器')}</strong><span>${count}</span></div>${incomplete}${content}<p class="mcp-servers-scope">${mcpServersText('Status reflects the current Desktop backend, not a live connection test. Running tasks may use separate processes. Configuration changes may require a restart.', '状态仅反映当前 Desktop 后台，未主动检测连接；运行中的任务可能使用独立进程。修改配置可能需要重启。')}</p></div>`;
}
function renderMcpServers() {
  const content = document.getElementById('settingsContent');
  if (!content) return;
  const state = mcpServersState;
  if (state.data) {
    content.innerHTML = mcpServersMarkup(state.data);
    if (state.error) content.insertAdjacentHTML('afterbegin', `<p class="mcp-servers-error" role="alert">${mcpServersText('Could not refresh MCP servers.', '无法刷新 MCP 服务器列表。')}</p>`);
    if (!state.loading && Date.now() - state.loadedAt > 5000) void loadMcpServers();
    return;
  }
  if (state.loading || !state.requested) {
    content.innerHTML = `<p class="mcp-servers-loading" role="status">${mcpServersText('Loading MCP servers…', '正在读取 MCP 服务器…')}</p>`;
    if (!state.loading) void loadMcpServers();
    return;
  }
  content.innerHTML = `<div class="mcp-servers-error" role="alert"><p>${mcpServersText('Could not load MCP servers.', '无法读取 MCP 服务器列表。')}</p><button type="button" class="mcp-servers-refresh" onclick="refreshMcpServers()">${mcpServersText('Retry', '重试')}</button></div>`;
}
async function loadMcpServers() {
  const state = mcpServersState, generation = ++state.generation;
  state.requested = true; state.loading = true; state.error = false;
  try {
    const response = await fetch('/api/mcp/servers');
    if (!response.ok) throw new Error('MCP list request failed');
    const data = await response.json();
    if (!data || !Array.isArray(data.servers)) throw new Error('Invalid MCP list response');
    if (generation !== state.generation) return;
    state.data = data;
    state.loadedAt = Date.now();
  } catch (_) {
    if (generation !== state.generation) return;
    // No raw network/server error: these may contain commands or credentials.
    state.error = true;
  } finally {
    if (generation === state.generation) {
      state.loading = false;
      if (mcpServersActive()) {
        // A failed request keeps old data without immediate retry recursion.
        if (state.error && state.data) state.loadedAt = Date.now();
        renderMcpServers();
      }
    }
  }
}
function refreshMcpServers() {
  void loadMcpServers();
  if (mcpServersActive()) renderMcpServers();
}
