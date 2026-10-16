'use strict';

/* 路由器软件中心（fnOS 移植版）的设置 / 状态面板。
 *
 * 面板本身由本应用的服务提供（页面前缀 /ctl/panel.html，相对路径 ../assets、../ctl/…），
 * 所以同一份文件在“fnOS 统一网关”和“本机调试端口”两种运行方式下都能用。
 *
 * 安全约定（与原版一致，判据换成 fnOS 管理员）：
 *   · 明文 AdminToken 只对已登录的 fnOS 管理员返回（GET /ctl/token）；
 *   · 非管理员只能改？—— 不能改：保存会被 403 拒绝，界面直接给出只读提示。
 *   · 令牌输入框永远不预填明文，避免把 token_hint 当令牌回存。
 */

const API = '../ctl';

const $ = (id) => document.getElementById(id);

/* fnOS 的桌面窗口大小由外壳决定（应用包里没有窗口尺寸配置），所以这里提供一个
   一键全屏按钮：把整个外壳放大到整屏。按钮由 assets/fullscreen.js 生成，
   样式跟面板自己的按钮一致（data-fs-inline=1）。 */
function loadFullscreenButton() {
  window.UnifiCenterFullscreen = window.UnifiCenterFullscreen ||
    { exists: () => false, toggle: () => {} };      // 真脚本加载前的占位
  if (document.querySelector('script[data-fs-target]')) return;
  const tag = document.createElement('script');
  tag.src = '../assets/fullscreen.js';
  tag.defer = true;
  tag.dataset.fsTarget = '#panelActions';
  tag.dataset.fsInline = '1';          // 用面板的内联按钮样式
  const host = $('panelActions');
  if (host) host.append(tag);
  else document.head.append(tag);
}

async function api(path, options) {
  const response = await fetch(`${API}/${path}`, {
    credentials: 'same-origin',
    cache: 'no-store',
    ...options,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(payload.error || `请求失败（HTTP ${response.status}）`);
    error.payload = payload;
    throw error;
  }
  return payload;
}

function toast(message, kind) {
  const node = $('toast');
  node.textContent = message;
  node.classList.toggle('is-error', kind === 'error');
  node.classList.remove('hidden');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.classList.add('hidden'), 3200);
}

function addRow(list, label, value, kind) {
  const dt = document.createElement('dt');
  dt.textContent = label;
  const dd = document.createElement('dd');
  dd.textContent = value;
  if (kind) dd.classList.add(`is-${kind}`);
  list.append(dt, dd);
}

let state = { settings: null, status: null, logsLoaded: false };

function applyPermission() {
  const settings = state.settings;
  const note = $('permNote');
  const editable = Boolean(settings && settings.can_edit);
  const user = (settings && settings.user) || {};
  const who = user.label || '未知身份';

  if (!settings) {
    note.textContent = '读不到设置：服务可能还没启动，或本页不是从 fnOS 桌面打开的。';
    return;
  }
  if (editable) {
    note.textContent = `当前身份：${who}（管理员）—— 可以修改地址与令牌。`;
    $('targetInput').disabled = false;
    $('tokenInput').disabled = false;
    $('saveSettings').disabled = false;
    $('revealToken').disabled = false;
    $('clearToken').classList.toggle('hidden', !settings.token_set);
    return;
  }
  note.textContent = `当前身份：${who}（非管理员，只读）—— 只有 fnOS 管理员可以修改地址与令牌；`
    + '明文令牌同样只对管理员可见。';
  $('targetInput').disabled = true;
  $('tokenInput').disabled = true;
  $('saveSettings').disabled = true;
  $('revealToken').disabled = true;
  $('clearToken').classList.add('hidden');
}

function renderStatus(payload) {
  const router = (payload && payload.router) || {};
  const dot = $('statusDot');
  dot.classList.toggle('is-ok', Boolean(router.reachable));
  dot.classList.toggle('is-bad', Boolean(payload) && !router.reachable);

  $('brandSub').textContent = router.reachable
    ? `${(router.title || '软件中心').replace(/\s*-\s*安全管理中心$/, '')}`
      + (typeof router.latency_ms === 'number' ? ` · ${router.latency_ms} ms` : '')
    : (payload ? `连不上目标：${router.error || '未知错误'}` : '正在探测…');

  const bits = [];
  if (payload && payload.target) bits.push(payload.target.replace(/^https?:\/\//, '').replace(/\/$/, ''));
  if (router.device) bits.push(router.device);
  if (router.version) bits.push(`v${router.version}`);
  $('routerMeta').textContent = bits.join(' · ');

  const list = $('statusList');
  list.replaceChildren();
  if (!payload) {
    addRow(list, '应用服务', '无法读取状态', 'bad');
    return;
  }
  addRow(list, '应用版本', payload.version || '—');
  addRow(list, '目标地址', payload.target || '—');
  addRow(list, '路由器', router.reachable ? `可达（HTTP ${router.status}）` : `不可达：${router.error || ''}`,
    router.reachable ? 'ok' : 'bad');
  addRow(list, '响应延迟', typeof router.latency_ms === 'number' ? `${router.latency_ms} ms` : '—');
  addRow(list, '软件中心', router.title || '—');
  addRow(list, 'AdminToken', payload.token_set
    ? (router.authenticated ? '已保存且有效' : '已保存（未验证）')
    : '未保存', payload.token_set ? 'ok' : 'warn');
  addRow(list, '网关前缀', payload.gateway_prefix || '—');
  // socket_ready 表示"本进程自己 bind 成功"；socket_present 只表示路径上有文件
  // —— 崩溃残留也会有文件，那种情况不能显示成"已就绪"
  if (payload.socket_ready) {
    addRow(list, 'Unix Socket', `${payload.socket_path}（已监听）`, 'ok');
  } else if (payload.socket_present) {
    addRow(list, 'Unix Socket', `${payload.socket_path}（有残留文件，但本进程没在监听）`, 'bad');
  } else {
    addRow(list, 'Unix Socket', `${payload.socket_path}（不存在）`, 'warn');
  }
  addRow(list, '配置目录', payload.config_dir || '—');
  addRow(list, '当前身份', ((payload.user || {}).label || '—')
    + ((payload.user || {}).is_admin ? '（管理员）' : '（只读）'));

  const assets = (payload.assets || {});
  const assetList = $('assetList');
  assetList.replaceChildren();
  const entries = Object.entries(assets);
  const missing = entries.filter(([, present]) => !present).map(([name]) => name);
  for (const [name, present] of entries) {
    addRow(assetList, name.replace(/^assets\//, ''), present ? '已本地化' : '缺失', present ? 'ok' : 'bad');
  }
  $('assetsNote').textContent = missing.length
    ? `缺失 ${missing.length} 个，页面可能显示不全`
    : '全部就绪';
}

async function loadStatus() {
  try {
    const payload = await api('status');
    state.status = payload;
    renderStatus(payload);
  } catch (error) {
    renderStatus(null);
  }
}

async function loadSettings() {
  try {
    const settings = await api('settings');
    state.settings = settings;
    const hint = settings.token_hint || '';
    $('targetInput').value = settings.target || '';
    $('tokenInput').value = '';
    $('tokenInput').placeholder = settings.token_set ? '已保存（留空则保持不变）' : '留空表示不保存';
    $('tokenNote').textContent = settings.token_set
      ? (settings.token_visible
        ? `已保存：${hint}（明文只对 fnOS 管理员可见）`
        : `已保存：${hint} —— 明文只对 fnOS 管理员可见；这里只能替换，看不到原值`)
      : '尚未保存 AdminToken：软件中心会要求你手动输入令牌。';
    $('settingsError').classList.add('hidden');
    applyPermission();
  } catch (error) {
    state.settings = null;
    $('settingsError').textContent = error.message;
    $('settingsError').classList.remove('hidden');
    applyPermission();
  }
}

async function loadLogs() {
  try {
    const payload = await api('logs');
    const lines = payload.lines || [];
    $('logBox').textContent = lines.length ? lines.join('\n') : '（暂无日志）';
    $('logBox').scrollTop = $('logBox').scrollHeight;
    state.logsLoaded = true;
  } catch (error) {
    $('logBox').textContent = `读不到日志：${error.message}`;
  }
}

async function saveSettings() {
  const button = $('saveSettings');
  const error = $('settingsError');
  error.classList.add('hidden');
  button.disabled = true;
  const payload = { target: $('targetInput').value.trim() };
  // 输入框为空就不回存：不能把 token_hint 当作令牌存回去
  if ($('tokenInput').value.trim()) payload.token = $('tokenInput').value.trim();
  try {
    renderStatus(await api('settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }));
    await loadSettings();
    toast('已保存');
  } catch (failure) {
    error.textContent = failure.message;
    error.classList.remove('hidden');
    if (failure.payload && failure.payload.router) renderStatus(failure.payload);
  } finally {
    if (state.settings && state.settings.can_edit) button.disabled = false;
  }
}

async function revealToken() {
  const button = $('revealToken');
  if (button.dataset.shown === '1') {
    button.dataset.shown = '0';
    button.textContent = '显示已保存的令牌';
    $('tokenInput').value = '';
    $('tokenInput').type = 'password';
    return;
  }
  try {
    const payload = await api('token');
    if (!payload.token) {
      toast('还没有保存令牌', 'error');
      return;
    }
    $('tokenInput').type = 'text';
    $('tokenInput').value = payload.token;
    button.dataset.shown = '1';
    button.textContent = '隐藏令牌';
    toast('已显示令牌（改完记得保存或点隐藏）');
  } catch (error) {
    toast(error.message, 'error');
  }
}

async function clearToken() {
  const button = $('clearToken');
  button.disabled = true;
  try {
    await api('settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: '' }),
    });
    $('tokenInput').value = '';
    $('tokenInput').type = 'password';
    $('revealToken').dataset.shown = '0';
    $('revealToken').textContent = '显示已保存的令牌';
    await loadSettings();
    await loadStatus();
    toast('令牌已清除');
  } catch (error) {
    toast(error.message, 'error');
  } finally {
    button.disabled = false;
  }
}

async function init() {
  loadFullscreenButton();
  await loadSettings();
  renderStatus(state.status);
  await Promise.all([loadStatus(), loadLogs()]);

  $('saveSettings').addEventListener('click', saveSettings);
  $('refreshStatus').addEventListener('click', async () => {
    await loadStatus();
    toast('已重新探测');
  });
  $('refreshLogs').addEventListener('click', loadLogs);
  $('revealToken').addEventListener('click', revealToken);
  $('clearToken').addEventListener('click', clearToken);

  setInterval(loadStatus, 20000);
  setInterval(() => { if (!state.logsLoaded) loadLogs(); }, 60000);
}

document.addEventListener('DOMContentLoaded', init);
