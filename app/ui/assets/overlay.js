'use strict';

/* 注入到路由器软件中心页面里的浮动按钮组（右上角一列）：
 *   ⚙  打开本应用的设置面板（/ctl/panel.html）
 *   ⛶  全屏（fnOS 的窗口大小由桌面外壳决定，应用包改不了，所以给一个一键放大）
 * 按钮用 DOM + CSSOM 创建，不依赖任何框架，也不覆盖页面自己的样式。 */

(function () {
  if (window.__unifiCenterFabLoaded) return;
  window.__unifiCenterFabLoaded = true;

  // 页面地址形如 /app/unificenter/ ，设置面板就在它的 ctl/ 下（相对当前目录）
  const PANEL_URL = 'ctl/panel.html';
  const FULLSCREEN_SRC = 'assets/fullscreen.js';

  const STYLE = `
.unifi-fab-col{position:fixed;right:12px;top:calc(60px + env(safe-area-inset-top,0px));
  z-index:2147483000;display:flex;flex-direction:column;gap:8px;align-items:center}
.unifi-fab{position:relative;width:38px;height:38px;border-radius:50%;
  border:1px solid rgba(255,255,255,.5);
  background:rgba(28,32,44,.66);color:#fff;font-size:17px;line-height:1;cursor:pointer;
  display:flex;align-items:center;justify-content:center;backdrop-filter:blur(8px);
  box-shadow:0 6px 18px rgba(0,0,0,.22);opacity:.5;transition:opacity .18s ease,transform .18s ease;
  padding:0}
.unifi-fab:hover{opacity:1;transform:scale(1.06)}
.unifi-fab:active{transform:scale(.94)}
@media (max-width:560px){.unifi-fab{width:34px;height:34px;font-size:15px;opacity:.72}
  .unifi-fab-col{top:calc(56px + env(safe-area-inset-top,0px));gap:6px}}
`;

  function loadFullscreen() {
    if (document.querySelector('script[data-fs-target]')) return;
    const tag = document.createElement('script');
    tag.src = FULLSCREEN_SRC;
    tag.defer = true;
    tag.dataset.fsTarget = '.unifi-fab-col';   // 按钮排进 ⚙ 下面那一格
    document.head.append(tag);
  }

  function mount() {
    if (document.querySelector('.unifi-fab-col')) return;
    const style = document.createElement('style');
    style.textContent = STYLE;
    document.head.append(style);

    const column = document.createElement('div');
    column.className = 'unifi-fab-col';

    const settings = document.createElement('button');
    settings.type = 'button';
    settings.className = 'unifi-fab';
    settings.title = 'unifi 软件中心 · 设置';
    settings.setAttribute('aria-label', 'unifi 软件中心 · 设置');
    settings.textContent = '⚙';
    settings.addEventListener('click', () => { window.location.href = PANEL_URL; });

    column.append(settings);
    document.body.append(column);
    loadFullscreen();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount);
  } else {
    mount();
  }
})();
