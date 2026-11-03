// Metis Desktop — thin bootstrap. Starts the loopback Web UI in a child frame
// so native Wails bindings remain available in this parent while the native
// client and browser build continue to share one UI codebase.
import {
  ChooseWorkspaceDirectory,
  GetUpdateProgress,
  GetUpdateStatus,
  InstallUpdateAndRestart,
  SetNativeTheme,
  StartInstallUpdateAndRestart,
  StartWebUI,
} from '../wailsjs/go/main/App.js';

window.addEventListener('load', async () => {
  const status = document.getElementById('boot-status');
  const boot = document.getElementById('boot');
  const frame = document.getElementById('metis-frame');
  let backendOrigin = '';
  let platform = '';
  let fullscreen = false;
  let chromeRevision = 0;
  let lastChromeMessage = '';
  let chromeTimer = null;

  const windowChrome = () => ({
    platform,
    overlay: platform === 'darwin',
    fullscreen,
    titlebarHeight: platform === 'darwin' ? 48 : 0,
    trafficLightInset: platform === 'darwin' && !fullscreen ? 96 : 0,
  });
  const publishWindowChrome = () => {
    if (!backendOrigin || platform !== 'darwin') return;
    const value = windowChrome();
    const serialized = JSON.stringify(value);
    if (serialized === lastChromeMessage) return;
    lastChromeMessage = serialized;
    frame.contentWindow.postMessage({ channel: 'metis-native', kind: 'window-chrome', value }, backendOrigin);
  };
  const refreshWindowChrome = async (notify = true) => {
    const revision = ++chromeRevision;
    if (platform === 'darwin' && typeof window.runtime?.WindowIsFullscreen === 'function') {
      try {
        const current = await window.runtime.WindowIsFullscreen();
        if (revision !== chromeRevision) return windowChrome();
        fullscreen = current === true;
      } catch (_) {
        // Preserve the last confirmed state if the native query is unavailable.
      }
    }
    if (notify) publishWindowChrome();
    return windowChrome();
  };
  const requireMacWindowGesture = (payload, detail) => {
    if (platform !== 'darwin' || fullscreen || payload.button !== 0 || payload.detail !== detail) {
      throw new Error('Window gesture is unavailable in this state.');
    }
  };
  const startWindowDrag = payload => {
    requireMacWindowGesture(payload, 1);
    if (payload.buttons !== 1 || typeof window.WailsInvoke !== 'function') {
      throw new Error('Window drag requires the primary mouse button.');
    }
    // Wails v2.12 records the native window's mouse-down before either frame
    // receives it. Its fixed "drag" handler checks that event still exists
    // and refuses fullscreen, then uses performWindowDragWithEvent. DOM events
    // in the loopback iframe do not reach the parent's CSS drag listener.
    // Keep this one protocol constant private; never forward a supplied invoke.
    window.WailsInvoke('drag');
    return true;
  };
  const toggleWindowMaximise = async payload => {
    await refreshWindowChrome(false);
    requireMacWindowGesture(payload, 2);
    if (typeof window.runtime?.WindowToggleMaximise !== 'function') {
      throw new Error('Native window zoom is unavailable.');
    }
    window.runtime.WindowToggleMaximise();
    return true;
  };
  // The bootstrap/error page remains a genuine parent-frame drag surface.
  // Only its empty chrome handles a double click; future controls stay usable.
  boot?.addEventListener?.('dblclick', event => {
    if (event.target?.closest?.('button, a, input, textarea, select, [role="button"]')) return;
    void toggleWindowMaximise({ button: event.button, detail: event.detail }).catch(() => {});
  });
  try {
    if (typeof window.runtime?.Environment === 'function') {
      const environment = await window.runtime.Environment();
      platform = String(environment?.platform || '');
    }
    await refreshWindowChrome(false);
    if (platform === 'darwin') {
      document.documentElement.dataset.nativeChrome = 'macos';
      window.addEventListener('resize', () => { void refreshWindowChrome(); });
      window.addEventListener('focus', () => { void refreshWindowChrome(); });
      // Enter/exit fullscreen may finish after the last resize event. Poll
      // the native state, emitting only changes, to remove/restore the inset.
      chromeTimer = window.setInterval(() => { void refreshWindowChrome(); }, 500);
      window.addEventListener('beforeunload', () => window.clearInterval(chromeTimer));
    }
    const url = await StartWebUI();
    backendOrigin = new URL(url).origin;
    const nativeActions = {
      'window-chrome': () => refreshWindowChrome(false),
      'window-drag': startWindowDrag,
      'window-toggle-maximise': toggleWindowMaximise,
      'choose-workspace': () => ChooseWorkspaceDirectory(),
      'check-update': () => GetUpdateStatus(),
      'install-update': () => InstallUpdateAndRestart(),
      'start-install-update': () => StartInstallUpdateAndRestart(),
      'get-update-progress': () => GetUpdateProgress(),
      'request-computer-use-permission': payload => {
        const kind = String(payload && payload.kind || '');
        if (kind !== 'accessibility' && kind !== 'screen-recording') {
          throw new Error('Unsupported Computer Use permission');
        }
        // Call the Wails binding in this GUI process. The iframe's backend
        // child cannot register METIS Desktop itself with macOS TCC.
        return window['go']['main']['App']['RequestComputerUsePermission'](kind);
      },
      'get-computer-use-permission-status': () => window['go']['main']['App']['GetComputerUsePermissionStatus'](),
      'set-theme': payload => {
        const theme = String(payload && payload.theme || 'auto');
        document.documentElement.dataset.theme = theme;
        return SetNativeTheme(theme);
      },
    };
    window.addEventListener('message', async event => {
      const request = event.data || {};
      if (event.source !== frame.contentWindow || event.origin !== backendOrigin || request.channel !== 'metis-native' || request.kind !== 'request') return;
      if (!Object.prototype.hasOwnProperty.call(nativeActions, request.action)) return;
      const action = nativeActions[request.action];
      try {
        const value = await action(request.payload || {});
        frame.contentWindow.postMessage({ channel: 'metis-native', kind: 'response', id: request.id, value }, backendOrigin);
      } catch (error) {
        frame.contentWindow.postMessage({ channel: 'metis-native', kind: 'response', id: request.id, error: String(error && error.message || error) }, backendOrigin);
      }
    });
    frame.addEventListener('load', () => {
      if (boot) boot.style.display = 'none';
      frame.style.display = 'block';
      lastChromeMessage = '';
      void refreshWindowChrome();
    }, { once: true });
    frame.src = url;
  } catch (err) {
    if (status) status.textContent = '启动失败: ' + err;
  }
});
