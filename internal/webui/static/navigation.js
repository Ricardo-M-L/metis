// Navigation belongs to the application, not to a modal or an iframe URL.
// Pages keep their DOM mounted so hiding the conversation never detaches SSE,
// clears the composer, or cancels a running turn.
(function () {
  'use strict';

  function normalizeRoute(value) {
    if (!value || !value.page) throw new Error('A navigation page is required');
    if (value.page === 'session') return {
      page: 'session', sessionId: value.sessionId || null,
      view: ['chat', 'trace', 'artifacts'].includes(value.view) ? value.view : 'chat'
    };
    if (value.page === 'settings') return { page: 'settings', tab: value.tab || 'general' };
    if (value.page === 'artifact') return {
      page: 'artifact', sessionId: value.sessionId || null,
      artifactId: String(value.artifactId || ''), version: Number(value.version) || 0
    };
    return { ...value };
  }

  // Kept independent from the DOM so asynchronous failures and history branch
  // semantics can be tested with the same implementation the Desktop uses.
  function createNavigation(options) {
    const pages = new Map();
    let entries = [normalizeRoute(options.initialRoute)];
    let index = 0;
    let generation = 0;
    let pending = null;
    const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
    const current = () => ({ ...entries[index] });
    const notify = () => options.changed?.(current(), {
      canBack: index > 0 && !pending,
      canForward: index < entries.length - 1 && !pending,
      pending: pending ? { ...pending.route } : null
    });
    function leave(previous, next) {
      if (previous.page !== next.page) pages.get(previous.page)?.leave?.(next);
    }
    function commit(route, opts) {
      const previous = current();
      leave(previous, route);
      if (opts.index !== undefined) index = opts.index;
      else if (opts.replace) entries[index] = route;
      else if (!same(previous, route)) {
        entries = entries.slice(0, index + 1);
        entries.push(route);
        index++;
      }
      notify();
    }
    async function navigate(value, opts = {}) {
      const route = normalizeRoute(value);
      const page = pages.get(route.page);
      if (!page) throw new Error('Unknown navigation page: ' + route.page);
      if (!pending && same(current(), route)) return true;
      const token = ++generation;
      pending = { route, token };
      options.start?.(route, current());
      notify();
      try {
        const accepted = await page.enter(route, current(), () => token === generation);
        if (token !== generation) return false;
        pending = null;
        if (accepted === false) {
          options.restore?.(current());
          notify();
          return false;
        }
        commit(route, opts);
        return true;
      } catch (error) {
        if (token !== generation) return false;
        pending = null;
        options.restore?.(current());
        notify();
        options.error?.(error);
        return false;
      }
    }
    return {
      register(name, hooks) { pages.set(name, hooks); },
      navigate, current,
      pending: () => pending ? { ...pending.route } : null,
      back() { return index > 0 && !pending ? navigate(entries[index - 1], { index: index - 1 }) : Promise.resolve(false); },
      forward() { return index < entries.length - 1 && !pending ? navigate(entries[index + 1], { index: index + 1 }) : Promise.resolve(false); },
      // A view that has already committed (e.g. a validated sidebar response)
      // records its selection without invoking its loader a second time.
      record(value, opts = {}) {
        const route = normalizeRoute(value);
        generation++;
        pending = null;
        options.recorded?.(route, current());
        commit(route, opts);
      },
      replaceSession(previousId, sessionId) {
        entries = entries.map(route => ['session', 'artifact'].includes(route.page) && route.sessionId === previousId
          ? { ...route, sessionId } : route);
        notify();
      },
      refresh: notify,
      snapshot: () => ({ entries: entries.map(route => ({ ...route })), index, pending: !!pending })
    };
  }

  window.createMetisNavigation = createNavigation;

  document.addEventListener('DOMContentLoaded', () => {
    const app = document.querySelector('.app');
    const main = document.querySelector('.main');
    if (!app || !main) return;
    let rendering = 0;
    const quietly = fn => {
      rendering++;
      try { return fn(); } finally { rendering--; }
    };
    const activeSession = () => typeof currentSessionId === 'undefined' ? null : currentSessionId;
    const activeView = () => typeof currentView === 'undefined' ? 'chat' : currentView;
    const text = (en, zh) => document.documentElement.lang === 'zh-CN' ? zh : en;
    const settingsNames = {
      general: ['General', '通用'], appearance: ['Appearance', '外观'],
      providers: ['Model providers', '模型提供商'], presets: ['Agent presets', '代理预设'],
      plugins: ['Plugins', '插件'], 'computer-use': ['Computer use', '电脑操作'],
      routing: ['Smart routing', '智能路由'], config: ['Configuration', '配置']
    };
    function invalidateSelection() {
      if (typeof invalidateSessionAsyncLoads === 'function') invalidateSessionAsyncLoads();
      // An artifact detail request may still be pending before active is set.
      // Every new navigation intent supersedes it, including Settings clicks.
      if (typeof artifactState !== 'undefined') artifactState.previewSequence++;
    }
    function showPage(route) {
      app.dataset.page = route.page;
      main.dataset.page = route.page;
      for (const [id, page] of [['settingsOverlay', 'settings'], ['automationsPage', 'schedules'], ['artifactPreviewOverlay', 'artifact']]) {
        const element = document.getElementById(id);
        if (element) element.hidden = route.page !== page;
      }
      if (route.page !== 'artifact' && typeof closeArtifactPreview === 'function') {
        quietly(() => closeArtifactPreview({ navigation: false }));
      }
    }
    function chrome(route, state) {
      const back = document.getElementById('navigationBack');
      const forward = document.getElementById('navigationForward');
      if (back) {
        back.disabled = !state.canBack;
        back.title = text('Back (Alt+Left)', '后退（Alt+←）');
        back.setAttribute('aria-label', back.title);
      }
      if (forward) {
        forward.disabled = !state.canForward;
        forward.title = text('Forward (Alt+Right)', '前进（Alt+→）');
        forward.setAttribute('aria-label', forward.title);
      }
      const header = document.getElementById('navigationHeader');
      if (header) header.setAttribute('aria-busy', String(!!state.pending));
      let label = text('New conversation', '新对话');
      if (route.page === 'settings') label = text('Settings', '设置') + ' / ' + text(...(settingsNames[route.tab] || settingsNames.general));
      else if (route.page === 'schedules') label = text('Scheduled tasks', '定时任务');
      else if (route.page === 'artifact') label = document.getElementById('artifactPreviewTitle')?.textContent || text('Artifact', '产物');
      else if (route.sessionId) {
        const session = typeof sessions !== 'undefined' && sessions.find(item => (item.id || item.ID) === route.sessionId);
        label = session && (session.title || session.name || session.Title) || text('Conversation', '对话');
        if (route.view === 'trace') label += ' / ' + text('Trajectory', '轨迹');
        if (route.view === 'artifacts') label += ' / ' + text('Artifacts', '产物');
      }
      const title = document.getElementById('navigationTitle');
      if (title) title.textContent = label;
      const schedules = document.getElementById('schedulesNav');
      if (schedules) {
        schedules.classList.toggle('active', route.page === 'schedules');
        schedules.title = text('Scheduled tasks', '定时任务');
        schedules.setAttribute('aria-label', schedules.title);
        const label = schedules.querySelector('span');
        if (label) label.textContent = schedules.title;
      }
      document.querySelector('.sb-settings')?.classList.toggle('active', route.page === 'settings');
    }

    const navigation = createNavigation({
      initialRoute: { page: 'session', sessionId: activeSession(), view: activeView() },
      start: invalidateSelection,
      recorded: showPage,
      changed(route, state) { if (!state.pending) showPage(route); chrome(route, state); },
      restore(route) {
        // Session activation commits before optional artifact loading. If the
        // target artifact is gone, retain the session that actually loaded;
        // never label B's transcript as the previous session A.
        if (['session', 'artifact'].includes(route.page) && route.sessionId !== activeSession()) {
          navigation.record({ page: 'session', sessionId: activeSession(), view: activeView() });
        } else showPage(route);
      },
      error: error => showToast(text('Unable to open page: ', '无法打开页面：') + error.message)
    });
    navigation.register('session', {
      async enter(route, _previous, isCurrent) {
        if (route.sessionId !== activeSession()) {
          if (route.sessionId) {
            const ok = await resumeSession(route.sessionId);
            if (!isCurrent() || ok !== true || route.sessionId !== activeSession()) return false;
          } else {
            quietly(() => newChat());
            if (activeSession() !== null) return false;
          }
        }
        if (!isCurrent()) return false;
        showPage(route);
        quietly(() => {
          if (route.view === 'artifacts') openArtifactsPanel();
          else if (activeView() !== route.view || route.view === 'trace') switchView(route.view);
        });
        return true;
      }
    });
    navigation.register('settings', {
      enter(route) {
        if (!settingsNames[route.tab]) throw new Error('Unknown settings page');
        showPage(route);
        // Loading configuration is independent of the navigation transaction.
        // A slow fetch must not block Back or undo a later settings selection.
        quietly(() => openSettings(route.tab, true));
        return true;
      }
    });
    navigation.register('schedules', {
      enter(route) { showPage(route); return typeof showAutomationsPage === 'function' ? showAutomationsPage() : true; },
      leave() { if (typeof hideAutomationsPage === 'function') hideAutomationsPage(); }
    });
    navigation.register('artifact', {
      async enter(route, _previous, isCurrent) {
        if (route.sessionId !== activeSession()) {
          const ok = await resumeSession(route.sessionId);
          if (!isCurrent() || ok !== true || route.sessionId !== activeSession()) return false;
        }
        await previewArtifactByID(route.artifactId, route.version);
        if (!isCurrent()) return false;
        if (typeof artifactState === 'undefined' || artifactState.active?.id !== route.artifactId) return false;
        showPage(route);
        return true;
      }
    });
    navigation.recordSession = (id, options = {}) => {
      if (rendering) return;
      const pending = navigation.pending();
      if (pending && ['session', 'artifact'].includes(pending.page) && pending.sessionId === (id || null)) return;
      // Assigning an id to the first turn also updates a conversation retained
      // behind Settings, without taking the user away from that page.
      if (options.replace && navigation.current().page !== 'session') {
        navigation.replaceSession(null, id);
        return;
      }
      // Session reset closes the old gallery/preview before this callback.
      // Coordinate the view variable and tabs with that committed surface;
      // the former gallery's currentView must not label a new chat as Artifacts.
      const view = main.classList.contains('artifacts-mode') ? 'artifacts'
        : main.classList.contains('trace-mode') ? 'trace' : 'chat';
      if (activeView() !== view) quietly(() => switchView(view));
      navigation.record({ page: 'session', sessionId: id, view }, options);
    };
    navigation.recordView = view => {
      if (rendering) return;
      const pending = navigation.pending();
      if (pending && pending.sessionId === activeSession() && (pending.page === 'artifact' || pending.view === view)) return;
      if (typeof pendingSessionId !== 'undefined' && pendingSessionId) invalidateSelection();
      navigation.record({ page: 'session', sessionId: activeSession(), view });
    };
    navigation.recordArtifact = (id, version) => {
      if (rendering) return;
      const pending = navigation.pending();
      if (pending?.page === 'artifact' && pending.artifactId === id) {
        // The detail is authoritative before the preview URL is ready. Show
        // its loading/error page immediately during a history restoration.
        showPage(pending);
        chrome(pending, { canBack: false, canForward: false, pending });
        return;
      }
      navigation.record({ page: 'artifact', sessionId: activeSession(), artifactId: id, version });
    };
    navigation.closeArtifact = () => {
      if (rendering || navigation.current().page !== 'artifact') return false;
      const history = navigation.snapshot();
      let target = history.index - 1;
      while (target >= 0 && history.entries[target].page === 'artifact') target--;
      if (target >= 0) void navigation.navigate(history.entries[target], { index: target });
      else void navigation.navigate({ page: 'session', sessionId: activeSession(), view: activeView() });
      return true;
    };
    navigation.closeSettings = () => {
      if (navigation.current().page !== 'settings') return;
      // The page's Back to app action returns to the entry point; the global
      // arrows continue to walk each visited settings subpage individually.
      const history = navigation.snapshot();
      let target = history.index - 1;
      while (target >= 0 && history.entries[target].page === 'settings') target--;
      if (target >= 0) void navigation.navigate(history.entries[target], { index: target });
      else void navigation.navigate({ page: 'session', sessionId: activeSession(), view: activeView() });
    };
    window.metisNavigation = navigation;
    navigation.refresh();

    function hasModal() {
      return Array.from(document.querySelectorAll('[aria-modal="true"]')).some(element =>
        !element.hidden && element.getClientRects().length > 0);
    }
    document.addEventListener('keydown', event => {
      if (event.defaultPrevented || event.shiftKey || event.ctrlKey || hasModal()) return;
      const altArrow = event.altKey && !event.metaKey && ['ArrowLeft', 'ArrowRight'].includes(event.key);
      const commandBracket = event.metaKey && !event.altKey && ['[', ']'].includes(event.key);
      if (!altArrow && !commandBracket) return;
      event.preventDefault();
      void (event.key === 'ArrowLeft' || event.key === '[' ? navigation.back() : navigation.forward());
    });
    // Mouse side buttons are handled locally; they must not unload the app's
    // loopback iframe (and with it its live event connection).
    document.addEventListener('mouseup', event => {
      if (event.button !== 3 && event.button !== 4 || hasModal()) return;
      event.preventDefault();
      void (event.button === 3 ? navigation.back() : navigation.forward());
    });
    document.addEventListener('auxclick', event => {
      if (event.button === 3 || event.button === 4) event.preventDefault();
    });
    if (typeof MutationObserver !== 'undefined') {
      new MutationObserver(() => navigation.refresh()).observe(document.documentElement, { attributes: true, attributeFilter: ['lang'] });
    }
  });
})();
