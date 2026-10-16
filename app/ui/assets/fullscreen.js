'use strict';

/* 全屏按钮：把整个 fnOS 桌面外壳放大到整屏。
 *
 * 背景：fnOS 的窗口大小由桌面外壳决定，应用包（manifest / app/ui/config）里没有任何
 * 窗口尺寸字段，所以"默认大一点"做不到；能给的是一个一键放大的入口。
 *
 * 兜底顺序：
 *   1. 标准 Fullscreen API：document.documentElement.requestFullscreen()
 *      —— 页面跑在桌面外壳的同源 iframe 里，请求会作用到顶层文档，也就是整个窗口；
 *   2. 老 WebKit 内核：webkitRequestFullscreen；
 *   3. 都不支持才提示用桌面窗口自己的最大化/全屏按钮（不猜宿主方法名，猜错了没意义）。
 *
 * 怎么用：其它脚本插入本文件时带上 data 属性即可，不需要关心加载顺序：
 *   <script src="../assets/fullscreen.js" defer
 *           data-fs-target=".unifi-fab-col"        ← 按钮插到哪个元素里（选择器）
 *           data-fs-position="static"               ← 可选：跟随宿主排版（默认 fixed 浮层）
 *           data-fs-top="..."                       ← 可选：fixed 时的 top
 *           data-fs-inline="1"                      ← 可选：用面板里的内联样式
 */

(function () {
  if (window.__unifiCenterFullscreenLoaded) return;
  window.__unifiCenterFullscreenLoaded = true;

  // 插入本脚本的标签（currentScript 在 defer 脚本里仍然有效）
  const tag = document.currentScript;

  function fullscreenElement() {
    return document.fullscreenElement || document.webkitFullscreenElement || null;
  }

  function supported() {
    const element = document.documentElement;
    return Boolean(element.requestFullscreen || element.webkitRequestFullscreen);
  }

  function toast(message) {
    const node = document.createElement('div');
    node.className = 'unifi-fs-toast';
    node.textContent = message;
    document.body.append(node);
    setTimeout(() => node.remove(), 3200);
  }

  function enter() {
    const element = document.documentElement;
    const request = element.requestFullscreen || element.webkitRequestFullscreen;
    if (!request) return false;
    try {
      const result = request.call(element, { navigationUI: 'hide' });
      if (result && typeof result.catch === 'function') {
        result.catch(() => { toast('浏览器拒绝了全屏请求，请用桌面窗口的全屏按钮'); });
      }
      return true;
    } catch (error) {
      return false;
    }
  }

  function exit() {
    const quit = document.exitFullscreen || document.webkitExitFullscreen;
    if (!quit) return false;
    try {
      const result = quit.call(document);
      if (result && typeof result.catch === 'function') result.catch(() => {});
      return true;
    } catch (error) {
      return false;
    }
  }

  function toggle() {
    if (fullscreenElement()) {
      exit();
      return;
    }
    if (!enter()) toast('当前环境不支持全屏，请用桌面窗口自己的全屏/最大化按钮');
  }

  const STYLE = `
.unifi-fs{position:fixed;right:12px;z-index:2147483001;
  width:38px;height:38px;border-radius:50%;border:1px solid rgba(255,255,255,.5);
  background:rgba(28,32,44,.66);color:#fff;font-size:15px;line-height:1;cursor:pointer;
  display:flex;align-items:center;justify-content:center;backdrop-filter:blur(8px);
  box-shadow:0 6px 18px rgba(0,0,0,.22);opacity:.5;transition:opacity .18s ease,transform .18s ease;
  padding:0}
.unifi-fs:hover{opacity:1;transform:scale(1.06)}
.unifi-fs:active{transform:scale(.94)}
/* 跟随宿主排版时（软件中心的按钮列）取消浮层定位，由宿主的 flex 排布 */
.unifi-fs.is-inline-position{position:relative;right:auto;top:auto !important}
/* 设置面板顶栏里的内联样式：跟面板按钮一致 */
.unifi-fs.is-panel{position:static;width:32px;height:32px;border-radius:9px;font-size:14px;
  border-color:var(--line,#e4e8ef);background:var(--panel,#fff);color:var(--text,#1c2430);
  box-shadow:none;opacity:1;backdrop-filter:none}
.unifi-fs.is-panel:hover{background:var(--accent-soft,rgba(47,111,237,.12));transform:none}
.unifi-fs-toast{position:fixed;left:50%;bottom:calc(22px + env(safe-area-inset-bottom,0px));
  transform:translateX(-50%);z-index:2147483600;max-width:76vw;
  background:rgba(16,20,32,.92);color:#fff;font:13px/1.4 -apple-system,BlinkMacSystemFont,
  "PingFang SC","Microsoft YaHei",sans-serif;padding:8px 14px;border-radius:10px;
  box-shadow:0 10px 26px rgba(0,0,0,.3)}
@media (max-width:560px){.unifi-fs{width:34px;height:34px;font-size:13px;opacity:.72}}
`;

  function ensureStyle() {
    if (document.querySelector('style[data-unifi-fullscreen]')) return;
    const style = document.createElement('style');
    style.setAttribute('data-unifi-fullscreen', '1');
    style.textContent = STYLE;
    document.head.append(style);
  }

  function describe() {
    return fullscreenElement() ? '退出全屏' : '全屏（放大窗口）';
  }

  function sync(button) {
    button.title = describe();
    button.setAttribute('aria-label', describe());
    button.textContent = fullscreenElement() ? '⤡' : '⛶';
  }

  function create(extraClass) {
    ensureStyle();
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'unifi-fs' + (extraClass ? ' ' + extraClass : '');
    sync(button);
    button.addEventListener('click', () => { toggle(); sync(button); });
    document.addEventListener('fullscreenchange', () => sync(button));
    document.addEventListener('webkitfullscreenchange', () => sync(button));
    return button;
  }

  // 对外暴露给页面脚本（按需再加载 / 主动查询时用）
  window.UnifiCenterFullscreen = {
    create,
    toggle,
    supported,
    exists: () => Boolean(document.querySelector('.unifi-fs')),
  };

  function mount() {
    const data = (tag && tag.dataset) || {};
    const target = data.fsTarget ? document.querySelector(data.fsTarget) : null;
    const host = target || document.body;
    if (host.querySelector('.unifi-fs')) return;

    let extra = '';
    if (data.fsInline === '1') extra = ' is-panel';
    else if (data.fsPosition === 'static' || target) extra = ' is-inline-position';

    const button = create(extra);
    if (!extra.includes('is-panel') && !extra.includes('is-inline-position')
        && data.fsTop) {
      button.style.top = data.fsTop;
    }
    host.append(button);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount);
  } else {
    mount();
  }
})();
