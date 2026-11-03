// Local aggregate statistics, loaded on demand from the Desktop backend.
// Request generations prevent a late refresh from overwriting newer data.
const usageStatisticsState = { data: null, error: '', loading: false, requested: false, generation: 0, period: 7, heatMode: 'daily' };

function usageStatisticsActive() {
  const overlay = document.getElementById('settingsOverlay');
  return !!overlay && !overlay.hidden && settingsTab === 'usage';
}

function usageText(en, zh) { return uiText(en, zh); }
function usageNumber(value) {
  return Number.isFinite(Number(value)) && Number(value) >= 0 ? Number(value) : 0;
}
function usageFormat(value) {
  return new Intl.NumberFormat(document.documentElement.lang === 'zh-CN' ? 'zh-CN' : 'en-US').format(usageNumber(value));
}
function usageDuration(value) {
  const seconds = Math.round(usageNumber(value) / 1000);
  if (seconds < 60) return seconds + usageText(' s', ' 秒');
  const minutes = Math.floor(seconds / 60), remainder = seconds % 60;
  if (minutes < 60) return minutes + usageText(' min', ' 分钟') + (remainder ? ' ' + remainder + usageText(' s', ' 秒') : '');
  return Math.floor(minutes / 60) + usageText(' h ', ' 小时 ') + (minutes % 60) + usageText(' min', ' 分钟');
}
function usageLocalDate(date) {
  const year = date.getFullYear(), month = String(date.getMonth() + 1).padStart(2, '0'), day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}
function usageDayOffset(date, days) {
  const result = new Date(date.getFullYear(), date.getMonth(), date.getDate(), 12);
  result.setDate(result.getDate() + days);
  return result;
}
function usageDaysByDate(data) {
  const map = new Map();
  for (const day of Array.isArray(data?.daily) ? data.daily : []) {
    if (day && /^\d{4}-\d{2}-\d{2}$/.test(day.date || '')) map.set(day.date, day);
  }
  return map;
}
function usageDayDescription(key, day) {
  if (!day) return key + ' · ' + usageText('No recorded activity', '无记录');
  return key + ' · ' + usageFormat(day.totalTokens) + ' tokens · ' + usageFormat(day.turns) + usageText(' turns', ' 次运行');
}
function usageHeatmap(days) {
  const today = new Date(), now = usageDayOffset(today, 0), mode = usageStatisticsState.heatMode;
  const first = usageDayOffset(now, -363), start = usageDayOffset(first, -first.getDay());
  const end = usageDayOffset(now, 6 - now.getDay());
  const firstKey = usageLocalDate(first), lastKey = usageLocalDate(now);
  const weeks = [], months = [], recorded = [];
  let previousMonth = -1, runningTokens = 0, runningTurns = 0, hasHistory = false;
  for (let date = start; date <= end; date = usageDayOffset(date, 7)) {
    const index = weeks.length, cells = [], month = date.getMonth();
    if (month !== previousMonth) {
      months.push(`<span style="grid-column:${index + 1}">${escHtml(new Intl.DateTimeFormat(document.documentElement.lang === 'zh-CN' ? 'zh-CN' : 'en-US', { month: 'short' }).format(date))}</span>`);
      previousMonth = month;
    }
    let total = 0, turns = 0, count = 0, lastVisibleKey = '';
    for (let dow = 0; dow < 7; dow++) {
      const day = usageDayOffset(date, dow), key = usageLocalDate(day);
      const outside = key < firstKey || key > lastKey, item = outside ? null : days.get(key);
      if (!outside) lastVisibleKey = key;
      if (item) { recorded.push({ key, day: item }); total += usageNumber(item.totalTokens); turns += usageNumber(item.turns); count++; }
      cells.push({ key, outside, item });
    }
    runningTokens += total; runningTurns += turns; hasHistory ||= count > 0;
    weeks.push({ cells, total, turns, count, lastVisibleKey, cumulative: runningTokens, cumulativeTurns: runningTurns, hasHistory });
  }
  const maxDaily = Math.max(0, ...recorded.map(({ day }) => usageNumber(day.totalTokens)));
  const maxWeekly = Math.max(0, ...weeks.map(w => w.total));
  const maxCumulative = Math.max(0, ...weeks.map(w => w.cumulative));
  const weekRows = [];
  const markup = weeks.map(week => {
    const value = mode === 'weekly' ? week.total : week.cumulative;
    const max = mode === 'weekly' ? maxWeekly : maxCumulative;
    const count = mode === 'weekly' ? week.turns : week.cumulativeTurns;
    const hasData = mode === 'weekly' ? week.count > 0 : week.hasHistory;
    const filled = value > 0 && max ? Math.min(7, Math.max(1, Math.ceil(value / max * 7))) : 0;
    const level = value > 0 && max ? Math.min(4, Math.max(1, Math.ceil(value / max * 4))) : 0;
    const startKey = week.cells.find(cell => !cell.outside)?.key || '';
    let label = '';
    if (mode !== 'daily') {
      const period = startKey + ' – ' + week.lastVisibleKey;
      label = mode === 'weekly'
        ? period + ' · ' + (week.count ? usageFormat(value) + ' tokens · ' + usageFormat(count) + usageText(' turns', ' 次运行') : usageText('No recorded activity', '无记录'))
        : usageText('Recorded cumulative through ', '截至 ') + week.lastVisibleKey + usageText(': ', ' 的已记录累计：') + (hasData ? usageFormat(value) + ' tokens · ' + usageFormat(count) + usageText(' turns', ' 次运行') : usageText('No recorded activity', '无记录')) + (hasData && !week.count ? ' · ' + usageText('No new records this week', '本周无新增记录') : '');
      if (hasData) weekRows.push(`<tr><th scope="row">${escHtml(period)}</th><td>${usageFormat(value)}</td><td>${usageFormat(count)}</td><td>${week.count ? usageFormat(week.count) : usageText('No records', '无记录')}</td></tr>`);
    }
    return week.cells.map((cell, row) => {
      if (mode === 'daily') {
        const item = cell.item, amount = item ? usageNumber(item.totalTokens) : 0;
        const cellLevel = item && amount > 0 && maxDaily > 0 ? Math.min(4, Math.max(1, Math.ceil(amount / maxDaily * 4))) : 0;
        const description = cell.outside ? '' : usageDayDescription(cell.key, item);
        return `<span class="usage-heat-cell${cell.outside ? ' is-outside' : item ? ' is-recorded' : ''}" data-level="${cellLevel}"${description ? ` title="${escAttr(description)}"` : ''}></span>`;
      }
      const isFilled = row >= 7 - filled;
      return `<span class="usage-heat-cell${isFilled ? ' is-recorded' : ''}" data-level="${isFilled ? level : 0}" title="${escAttr(label)}"></span>`;
    }).join('');
  }).join('');
  const tableRows = mode === 'daily'
    ? recorded.map(({ key, day }) => `<tr><th scope="row">${escHtml(key)}</th><td>${usageFormat(day.totalTokens)}</td><td>${usageFormat(day.turns)}</td></tr>`).join('')
    : weekRows.join('');
  const columns = mode === 'daily' ? '' : `<th scope="col">${usageText('Recorded days', '有记录天数')}</th>`;
  const accessible = tableRows ? `<details class="usage-data-table"><summary>${usageText('View 52-week activity as a table', '以表格查看 52 周活动')}</summary><table><thead><tr><th scope="col">${usageText(mode === 'daily' ? 'Date' : 'Week', mode === 'daily' ? '日期' : '周')}</th><th scope="col">Token</th><th scope="col">${usageText('Turns', '运行次数')}</th>${columns}</tr></thead><tbody>${tableRows}</tbody></table></details>` : '';
  return `<div class="usage-heat-scroll" tabindex="0" aria-label="${escAttr(usageText('Scroll to view activity by day', '滚动查看每日活动'))}"><div class="usage-heat-inner" style="--usage-weeks:${weeks.length}"><div class="usage-months" aria-hidden="true">${months.join('')}</div><div class="usage-heatmap" aria-hidden="true">${markup}</div></div></div>${accessible}`;
}

function usageTrend(days, period) {
  const now = new Date(), rows = [];
  for (let i = period - 1; i >= 0; i--) {
    const date = usageDayOffset(now, -i), key = usageLocalDate(date);
    rows.push({ key, day: days.get(key) });
  }
  const max = Math.max(0, ...rows.map(row => row.day ? usageNumber(row.day.totalTokens) : 0));
  const recorded = rows.filter(row => row.day);
  if (!recorded.length) return `<div class="usage-empty">${usageText('No recorded activity in this range.', '当前时间范围内暂无记录。')}</div>`;
  const bars = rows.map(({ key, day }, i) => {
    const n = day ? usageNumber(day.totalTokens) : 0;
    const height = day && n && max ? Math.max(4, Math.round(100 * n / max)) : 0;
    const label = usageDayDescription(key, day);
    const showLabel = period === 7 || i === 0 || i === rows.length - 1 || i % 7 === 0;
    const edge = i < 3 ? ' is-start' : i >= rows.length - 3 ? ' is-end' : '';
    const accessible = day ? ` tabindex="0" role="img" aria-label="${escAttr(label)}"` : '';
    return `<div class="usage-trend-day${edge}"${accessible}><div class="usage-trend-track">${day ? `<span class="usage-trend-bar" style="height:${height}%"></span>` : '<span class="usage-trend-missing"></span>'}</div><span class="usage-trend-label" aria-hidden="true">${showLabel ? escHtml(key.slice(5)) : '&nbsp;'}</span><span class="usage-trend-tooltip" aria-hidden="true"><span>${escHtml(key)}</span><strong>${day ? usageFormat(n) + ' Token' : usageText('No recorded activity', '无记录')}</strong></span></div>`;
  }).join('');
  return `<div class="usage-trend-chart" role="group" aria-label="${escAttr(usageText('Daily token trend', '每日 Token 趋势'))}">${bars}</div>`;
}
function usageCoverage(data) {
  const c = data.coverage || {}, parts = [];
  parts.push(usageText(`Local sessions: ${usageFormat(c.sessionsWithUsage)} of ${usageFormat(c.sessions)} have saved totals.`, `本机 ${usageFormat(c.sessions)} 个会话中，${usageFormat(c.sessionsWithUsage)} 个有用量汇总记录。`));
  if (usageNumber(c.unreadableSessions)) parts.push(usageText(`${usageFormat(c.unreadableSessions)} session files could not be read; results are incomplete.`, `${usageFormat(c.unreadableSessions)} 个会话文件无法读取，统计不完整。`));
  if (usageNumber(c.metricOnlySessions)) parts.push(usageText(`${usageFormat(c.metricOnlySessions)} sessions have turn details but no authoritative total; their recorded amounts are a lower bound.`, `${usageFormat(c.metricOnlySessions)} 个会话缺少权威总量，仅按已记录的逐轮数据计入下限。`));
  if (usageNumber(c.sessionsWithoutUsage)) parts.push(usageText(`${usageFormat(c.sessionsWithoutUsage)} sessions have no saved session total.`, `${usageFormat(c.sessionsWithoutUsage)} 个会话缺少用量汇总记录。`));
  if (usageNumber(c.modelUnreadableSessions)) parts.push(usageText(`${usageFormat(c.modelUnreadableSessions)} session model records could not be read.`, `${usageFormat(c.modelUnreadableSessions)} 个会话的模型记录无法读取。`));
  if (usageNumber(c.modelInconsistentSessions)) parts.push(usageText(`${usageFormat(c.modelInconsistentSessions)} sessions have model records inconsistent with turn details or saved totals.`, `${usageFormat(c.modelInconsistentSessions)} 个会话的模型记录与逐轮明细或汇总不一致。`));
  if (usageNumber(c.unattributedModelTokens)) parts.push(usageText(`${usageFormat(c.unattributedModelTokens)} tokens have no saved model attribution and are excluded from the model breakdown.`, `${usageFormat(c.unattributedModelTokens)} 个 Token 缺少模型归属，不计入模型用量。`));
  if (usageNumber(c.unattributedTokens)) parts.push(usageText(`${usageFormat(c.unattributedTokens)} tokens cannot be assigned to a date and are excluded from daily charts.`, `${usageFormat(c.unattributedTokens)} 个 Token 无法归属日期，不计入每日图表。`));
  if (usageNumber(c.inconsistentSessions)) parts.push(usageText(`${usageFormat(c.inconsistentSessions)} sessions have inconsistent totals and turn details.`, `${usageFormat(c.inconsistentSessions)} 个会话的总量与逐轮明细不一致。`));
  if (usageNumber(c.invalidDurationTurns)) parts.push(usageText(`${usageFormat(c.invalidDurationTurns)} turns lack a valid duration or exceed numeric limits; session duration may be incomplete.`, `${usageFormat(c.invalidDurationTurns)} 个轮次缺少有效时长或超出数值范围，会话时长可能不完整。`));
  if (usageNumber(c.invalidMetricTurns)) parts.push(usageText(`${usageFormat(c.invalidMetricTurns)} invalid turn records were excluded.`, `${usageFormat(c.invalidMetricTurns)} 条无效的逐轮记录已排除。`));
  parts.push(usageText('Daily charts, peaks and streaks use dated turn details only; historical totals may not have date details.', '每日图表、峰值和连续天数仅基于有日期的逐轮明细；历史总量可能没有日期明细。'));
  parts.push(usageText('Session duration sums recorded completed turns and may be incomplete; it is not wall-clock elapsed time.', '会话时长仅累加已记录的已完成轮次，可能不完整，并非墙钟经过时间。'));
  return parts.map(line => `<p>${escHtml(line)}</p>`).join('');
}
function usageModelBreakdown(data) {
  const models = Array.isArray(data.models) ? data.models.filter(m => m && typeof m.model === 'string' && m.model.trim()) : [];
  if (data.modelBreakdownAvailable !== true || !models.length) {
    return `<div class="usage-empty">${usageText('Past records do not store the model for every request, so model breakdown is not available.', '历史记录未保存每次请求的模型，暂无法按模型统计。')}</div>`;
  }
  const max = Math.max(0, ...models.map(m => usageNumber(m.totalTokens)));
  return `<div class="usage-models">${models.map(m => {
    const value = usageNumber(m.totalTokens), width = max ? Math.round(value / max * 100) : 0;
    return `<div class="usage-model"><div class="usage-model-label"><span>${escHtml(m.model)}</span><strong>${usageFormat(value)} tokens</strong></div><div class="usage-model-track" aria-hidden="true"><span style="width:${width}%"></span></div></div>`;
  }).join('')}</div>`;
}
function usageMetric(label, value) { return `<div class="usage-metric"><span class="usage-metric-value">${escHtml(value)}</span><span class="usage-metric-label">${escHtml(label)}</span></div>`; }
function usageStatisticsMarkup(data) {
  const total = data.total || {}, c = data.coverage || {}, available = usageNumber(c.sessionsWithUsage) > 0 || usageNumber(c.metricOnlySessions) > 0;
  const days = usageDaysByDate(data), period = usageStatisticsState.period;
  const metric = value => available ? usageFormat(value) : '—';
  const dayUnit = usageText(' days', ' 天');
  const metrics = usageMetric(usageText('Total tokens', '累计 Token 数'), metric(total.totalTokens))
    + usageMetric(usageText('Peak day tokens', '单日峰值 Token'), days.size ? usageFormat(data.peakDayTokens) : '—')
    + usageMetric(usageText('Longest session', '最长会话'), usageNumber(c.sessionsWithDuration) > 0 ? usageDuration(data.longestSessionMs) : '—')
    + usageMetric(usageText('Current streak', '当前连续天数'), days.size ? usageFormat(data.currentStreak) + dayUnit : '—')
    + usageMetric(usageText('Longest streak', '最长连续天数'), days.size ? usageFormat(data.longestStreak) + dayUnit : '—');
  return `<div class="usage-dashboard"><header class="usage-heading"><div><h2>${usageText('Usage statistics', '使用统计')}</h2><p>${usageText('Recorded locally across all sessions', '本机所有会话的已记录用量')}</p></div><button type="button" class="usage-refresh" onclick="refreshUsageStatistics()" ${usageStatisticsState.loading ? 'disabled' : ''}>${usageStatisticsState.loading ? usageText('Refreshing…', '正在刷新…') : usageText('Refresh', '刷新')}</button></header>
    <section class="usage-metrics" aria-label="${escAttr(usageText('Usage overview', '使用概览'))}">${metrics}</section>
    <section class="usage-panel"><div class="usage-panel-head"><div><h3>${usageText('Token activity', 'Token 活动')}</h3><p>${usageStatisticsState.heatMode === 'cumulative' ? usageText('Cumulative recorded activity within the last 52 weeks · local time', '近 52 周内有日期的已记录累计 · 本地时间') : usageStatisticsState.heatMode === 'weekly' ? usageText('Calendar weeks, limited to the last 52 weeks · local time', '按自然周汇总，起止周可能不完整 · 本地时间') : usageText('Last 52 weeks · local time', '最近 52 周 · 本地时间')}</p></div><div class="usage-period" role="group" aria-label="${escAttr(usageText('Activity display', '活动显示方式'))}"><button type="button" id="usageHeatDaily" aria-pressed="${usageStatisticsState.heatMode === 'daily'}" onclick="setUsageHeatMode('daily')">${usageText('Daily', '每日')}</button><button type="button" id="usageHeatWeekly" aria-pressed="${usageStatisticsState.heatMode === 'weekly'}" onclick="setUsageHeatMode('weekly')">${usageText('Weekly', '每周')}</button><button type="button" id="usageHeatCumulative" aria-pressed="${usageStatisticsState.heatMode === 'cumulative'}" onclick="setUsageHeatMode('cumulative')">${usageText('Cumulative', '累计')}</button></div></div>${usageHeatmap(days)}<div class="usage-legend">${usageStatisticsState.heatMode === 'daily' ? `<span>${usageText('No record', '无记录')}</span><span class="usage-heat-cell"></span><span class="usage-heat-cell is-recorded" data-level="0"></span><span>${usageText('Recorded, 0 tokens', '有记录，0 Token')}</span>` : `<span>${usageText('Recorded data only; missing days are unknown.', '仅统计已记录数据，缺失日期为未知。')}</span>`}<span>${usageText('Less', '少')}</span><span class="usage-heat-cell is-recorded" data-level="1"></span><span class="usage-heat-cell is-recorded" data-level="2"></span><span class="usage-heat-cell is-recorded" data-level="3"></span><span class="usage-heat-cell is-recorded" data-level="4"></span><span>${usageText('More', '多')}</span></div></section>
    <section class="usage-panel"><div class="usage-panel-head"><div><h3>${usageText('Daily token trend', '每日 Token 趋势')}</h3><p>${usageText('Days without records are left blank', '未记录的日期留空')}</p></div><div class="usage-period" role="group" aria-label="${escAttr(usageText('Time range', '时间范围'))}"><button type="button" id="usagePeriod7" aria-pressed="${period === 7}" onclick="setUsageStatisticsPeriod(7)">${usageText('7 days', '近 7 日')}</button><button type="button" id="usagePeriod30" aria-pressed="${period === 30}" onclick="setUsageStatisticsPeriod(30)">${usageText('30 days', '近 30 日')}</button></div></div>${usageTrend(days, period)}</section>
    <section class="usage-panel usage-breakdown"><h3>${usageText('Token breakdown', 'Token 分类')}</h3>${available ? `<dl><div><dt>${usageText('Input', '输入')}</dt><dd>${usageFormat(total.inputTokens)}</dd></div><div><dt>${usageText('Output', '输出')}</dt><dd>${usageFormat(total.outputTokens)}</dd></div><div><dt>${usageText('Cache read', '缓存读取')}</dt><dd>${usageFormat(total.cacheReadTokens)}</dd></div><div><dt>${usageText('Cache creation', '缓存写入')}</dt><dd>${usageFormat(total.cacheCreateTokens)}</dd></div></dl>` : `<div class="usage-empty">${usageText('No token records yet.', '暂无 Token 记录。')}</div>`}</section>
    <section class="usage-panel"><h3>${usageText('Usage by model', '模型用量')}</h3>${usageModelBreakdown(data)}</section>
    <section class="usage-coverage"><h3>${usageText('About this data', '数据说明')}</h3>${usageCoverage(data)}</section></div>`;
}
function renderUsageStatistics() {
  const content = document.getElementById('settingsContent');
  if (!content) return;
  const state = usageStatisticsState;
  if (state.data) {
    content.innerHTML = usageStatisticsMarkup(state.data);
    if (state.error) content.insertAdjacentHTML('afterbegin', `<p class="usage-error" role="alert">${escHtml(state.error)}</p>`);
    return;
  }
  if (state.loading || !state.requested) {
    content.innerHTML = `<div class="usage-loading" role="status">${usageText('Loading local usage statistics…', '正在读取本机使用统计…')}</div>`;
    if (!state.loading) void loadUsageStatistics();
    return;
  }
  content.innerHTML = `<div class="usage-error" role="alert"><p>${usageText('Could not load usage statistics.', '无法读取使用统计。')}</p><p>${escHtml(state.error)}</p><button type="button" class="usage-refresh" onclick="refreshUsageStatistics()">${usageText('Retry', '重试')}</button></div>`;
}
async function loadUsageStatistics() {
  const state = usageStatisticsState, generation = ++state.generation;
  state.requested = true; state.loading = true; state.error = '';
  try {
    const response = await fetch('/api/usage/statistics');
    if (!response.ok) throw new Error(usageText('Request failed', '请求失败') + ' (' + response.status + ')');
    const data = await response.json();
    if (!data || !Array.isArray(data.daily) || !data.total || !data.coverage) throw new Error(usageText('Invalid usage response', '统计数据格式无效'));
    if (generation !== state.generation) return;
    state.data = data;
  } catch (error) {
    if (generation !== state.generation) return;
    state.error = String(error?.message || error);
  } finally {
    if (generation === state.generation) {
      state.loading = false;
      if (usageStatisticsActive()) renderUsageStatistics();
    }
  }
}
function refreshUsageStatistics() {
  void loadUsageStatistics();
  if (usageStatisticsActive()) renderUsageStatistics();
}
function setUsageStatisticsPeriod(period) {
  if (period !== 7 && period !== 30) return;
  usageStatisticsState.period = period;
  if (usageStatisticsActive()) {
    renderUsageStatistics();
    document.getElementById('usagePeriod' + period)?.focus();
  }
}

function setUsageHeatMode(mode) {
  if (!['daily', 'weekly', 'cumulative'].includes(mode)) return;
  usageStatisticsState.heatMode = mode;
  if (usageStatisticsActive()) {
    renderUsageStatistics();
    document.getElementById('usageHeat' + mode[0].toUpperCase() + mode.slice(1))?.focus();
  }
}
