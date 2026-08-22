// Metis Desktop — thin bootstrap. Starts the in-process web UI backend
// (metis desktop --web) and navigates the webview straight to it, so the
// native client and the browser build share ONE UI codebase.
import { StartWebUI } from '../wailsjs/go/main/App.js';

window.addEventListener('load', async () => {
  const status = document.getElementById('boot-status');
  try {
    const url = await StartWebUI();
    window.location.replace(url);
  } catch (err) {
    if (status) status.textContent = '启动失败: ' + err;
  }
});
