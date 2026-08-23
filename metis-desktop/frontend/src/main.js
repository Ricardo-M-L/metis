// Metis Desktop — thin bootstrap. Starts the loopback Web UI in a child frame
// so native Wails bindings remain available in this parent while the native
// client and browser build continue to share one UI codebase.
import {
  ChooseWorkspaceDirectory,
  GetUpdateStatus,
  InstallUpdateAndRestart,
  StartWebUI,
} from '../wailsjs/go/main/App.js';

window.addEventListener('load', async () => {
  const status = document.getElementById('boot-status');
  const boot = document.getElementById('boot');
  const frame = document.getElementById('metis-frame');
  try {
    const url = await StartWebUI();
    const backendOrigin = new URL(url).origin;
    const nativeActions = {
      'choose-workspace': () => ChooseWorkspaceDirectory(),
      'check-update': () => GetUpdateStatus(),
      'install-update': () => InstallUpdateAndRestart(),
    };
    window.addEventListener('message', async event => {
      const request = event.data || {};
      if (event.source !== frame.contentWindow || event.origin !== backendOrigin || request.channel !== 'metis-native' || request.kind !== 'request') return;
      const action = nativeActions[request.action];
      if (!action) return;
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
    }, { once: true });
    frame.src = url;
  } catch (err) {
    if (status) status.textContent = '启动失败: ' + err;
  }
});
