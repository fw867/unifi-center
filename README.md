# unifi 软件中心（飞牛 fnOS 应用 · unificenter）

> 开发者：**fw867** ｜ 应用标识 `appname` 固定为 `unificenter`（保持稳定，改显示名不影响覆盖安装）。

把局域网里**路由器上的软件中心**（UniFi SoftCenter、KoolCenter 类的插件中心，默认
`http://192.168.1.1:9958/`）搬进飞牛 fnOS：**在 fnOS 桌面里直接打开它、装插件、看日志、改定时任务**。

两个关键点：

1. **不需要给路由器做任何端口映射**。插件中心在 NAS 上做反向代理，页面与接口都经
   fnOS 的桌面入口（NAS 域名 + NAS 登录态）转发到路由器，路由器始终待在局域网里。
2. **安装包零运行时依赖**。服务端是 `CGO_ENABLED=0` 编译出来的 **静态 Go 二进制**，
   目标机不需要 Python、Node、JDK，也不需要 `install_dep_apps` 装任何依赖应用，
   包里也没有 `.so`/解释器 —— 解包即跑。

```
浏览器 / fnOS 桌面
   │  NAS 登录态（统一网关先校验会话）
   ▼
fnOS 网关  /app/unificenter/…
   │  转发到 Unix Socket
   ▼
/var/apps/unificenter/target/app.sock  ← app/bin/unifi-center（静态二进制）
   ├── /                 软件中心页面（取回后本地化，不套 iframe）
   ├── /api/…            软件中心的接口 → 路由器（Authorization 按规则取值）
   ├── /github/…         页面里对 GitHub 的直连 → NAS 转发
   ├── /ctl/…            本应用自己的接口（状态、设置、令牌、日志、设置面板）
   └── /assets/…         本地资源（Vue / Tailwind / Lucide / 关闭按钮 / 设置按钮）
```

## 功能

- **页面前缀就是软件中心**（不套 iframe）：页面由本应用取回后，把三个外部 CDN 换成本地副本、
  把直连 GitHub 的地址改走 NAS，因此在**只能访问 NAS 域名、访问不了外网**的桌面窗口里也能完整显示；
- **自动带令牌**：AdminToken 存在 NAS 上（权限 0600），打开页面时自动写进同源的
  `localStorage.sc_token`，软件中心一加载就通过鉴权 —— 不用每次重输；
- **转发时不丢令牌**：转发到路由器的 `Authorization` 按三条规则取值 —— 客户端自己带的真实令牌
  原样透传；控制台那一跳塞的占位值 `Bearer console-loopback` 换成本应用保存的令牌；
  完全没带凭据时也用保存的令牌顶上（都没保存才不带，让软件中心照实报 401）；
- **关闭按钮**：左上角中间一个图形按钮，桌面窗口里调宿主方法 `normal_goback` 关闭当前应用页，
  非宿主环境退回 `history.back()` / `window.close()`；
- **设置面板** `/ctl/panel.html`（页面右上角 ⚙ 进入）：改目标地址、看路由器可达性/延迟/版本、
  令牌状态、本地资源与最近日志；
- **目标地址可改**：默认 `http://192.168.1.1:9958/`，改完立即生效（路由器接口由本应用转发，
  不涉及任何系统配置改动，也不会动路由器上任何东西）；
- **只读采集**：状态探测只发两个 GET（首页 + `/api/system/info`），不改动路由器；
- 体积小：静态二进制约 6.8 MB（x86）/ 6.3 MB（arm），`.fpk` 约 3.1 MB / 2.9 MB。

## 安装

```powershell
# Windows 开发机（两个架构一起出）
powershell -File deploy\build.ps1

# Linux / macOS
bash deploy/build.sh
```

产出：

| 文件 | platform | 二进制 | 适用设备 |
| --- | --- | --- | --- |
| `dist/unificenter-1.0.0-x86.fpk` | `x86` | linux/amd64 | x86_64 的 fnOS 设备 |
| `dist/unificenter-1.0.0-arm.fpk` | `arm` | linux/arm64 | ARM64 的 fnOS 设备 |

装到设备上（**选对架构**：包里是特定架构的二进制，装错了服务起不来）：

```bash
appcenter-cli install-fpk unificenter-1.0.0-x86.fpk     # 或 --env config.env 带向导默认值
appcenter-cli list                                      # 确认 unificenter
appcenter-cli start unificenter
```

装好后在 **fnOS 桌面 → 路由器软件中心** 打开；右上角 ⚙ 是设置面板。
向导（安装时）可先填路由器地址与 AdminToken，留空也行，装好后在设置面板里改。

### 起不来时先跑 doctor

`cmd/main` 会自己找应用文件目录（见下），出问题时有专门的排障子命令：

```bash
/var/apps/unificenter/cmd/main doctor
```

它会打印 `TRIM_*` 环境变量、解析出的应用目录、每一个候选布局里有什么（`bin` / `ui` / `sock`）、
进程状态和最近 20 行服务日志 —— 一句话就能看出设备上的真实布局。

### 关于 TRIM_APPDEST 的两种布局

官方文档写 `TRIM_APPDEST` 是"已安装应用的 **target** 目录"，但**实测有设备把它指到应用根目录**
（即 target 的上一级）。为了两种都能跑，`cmd/main` 不认死一种布局，而是按顺序试
`$TRIM_APPDEST` → `$TRIM_APPDEST/target` → `/var/apps/<appname>/target` → `/var/apps/<appname>`，
挑**真正有 `bin/unifi-center`** 的那个；Go 服务端启动时用同一套规则解析 `ui/`。

**Socket 是唯一的例外**：它必须留在 `$TRIM_APPDEST/app.sock`，因为统一网关按 `TRIM_APPDEST`
找它（入口配置里的 `gatewaySocket=app.sock`）—— 放到别处桌面入口就会 502。

### 包结构必须和官方 fnpack 产物一致（踩过大坑）

`app.tgz` 的**顶层直接是 `bin/`、`ui/`、`config/`**，不带 `app/` 这一层 —— 官方的 `app.tgz`
就是这样，安装器把它解到 target 下，于是装完是 `target/bin/…`、`target/ui/…`。
最初手写的打包器把整个 `app/` 目录塞了进去，装完变成 `target/app/bin/…`，结果应用中心报
**"无法启用：找不到可执行文件"**，`ui/` 也不存在。

所以 `tools/pack_fpk.py` 有两道自检：`check_app_layout()` 会拒绝带 `app/` 前缀的包，
并要求 `bin/unifi-center`、`ui/config`、`config/privilege`、`config/resource` 都在；
`TestBuiltPackages` 在 `go test` 里再核对一次。用官方工具打的包可以这样对比：

```bash
fnpack build --directory .     # 官方产物
python - <<'PY'                # 对比两个 .fpk 的 app.tgz 条目集合
import io, tarfile
def entries(p):
    with tarfile.open(p, 'r:gz') as t: data = t.extractfile('app.tgz').read()
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as i:
        return {m.name.rstrip('/') for m in i.getmembers()}
print(entries('unificenter.fpk') == entries('dist/unificenter-1.0.0-x86.fpk'))
PY
```

> 官方 `app.tgz` 里文件权限记的是 0666，本项目的打包器故意记 0755：
> 目标机要直接执行 `bin/unifi-center` 与 `cmd/*`，可执行位在包里就在，最稳。

## 目录结构

```
unifi-center/
├── manifest                  # 应用元数据（键值文件，无扩展名）
├── ICON.PNG / ICON_256.PNG   # 包图标 64×64 / 256×256
├── service/                  # Go 服务端源码（标准库，无第三方依赖）
│   ├── main.go               # 子命令：serve / seed / migrate-legacy / status / selftest / version
│   ├── engine.go             # 配置与令牌、目标地址规整、Authorization 决策、页面重写、探测
│   ├── server.go             # 路由、转发、静态资源、Unix Socket 监听
│   ├── main_test.go          # 单元 + HTTP 层测试（httptest）
│   └── package_test.go       # 构建产物验证（ELF 架构、静态链接、包内权限位）
├── app/
│   ├── bin/                  # 构建时放入的静态二进制（安装后 → $TRIM_APPDEST/bin/）
│   └── ui/                   # 桌面入口 UI（安装后 → $TRIM_APPDEST/ui/）
│       ├── config            # 入口注册（统一网关）
│       ├── panel.html/.js/.css   # 设置面板
│       ├── images/icon_{64,256}.png
│       └── assets/           # 本地 CDN 副本、图标、关闭/设置按钮脚本
├── cmd/                      # 生命周期脚本（main + install/upgrade/uninstall/config 钩子）
├── config/{privilege,resource}
├── wizard/{install,upgrade,uninstall,config}
├── tools/pack_fpk.py         # 打包 .fpk（标准库，精确控制包内权限）
├── tools/make_icon.py        # 生成图标（标准库，不依赖 Pillow）
├── tools/ui-apple-touch-icon.png  # 图标源：ui.com 官方 256×256
├── tools/ui-favicon.ico      # 备选图标源：ui.com 官方 favicon（最大 48×48）
├── tools/smoke.sh            # 端到端冒烟（Linux/WSL：解包 + 真跑 + 经 Socket 打接口）
└── legacy/python/            # 最初的 Python 实现，仅作参考，不参与构建
```

## 窗口大小（fnOS 桌面窗口）

**结论：窗口大小改不了。** 我把[应用入口](https://developer.fnnas.com/docs/core-concepts/app-entry/)、
[Manifest](https://developer.fnnas.com/docs/core-concepts/manifest/) 的字段逐条核对过：
入口配置里**没有任何窗口尺寸字段**（只有 `title`/`icon`/`type`/`protocol`/`port`/`url`/
`allUsers`/`fileTypes`/`noDisplay`/`control`/`gatewayPrefix`/`gatewaySocket`）。
`type=iframe` 时窗口由 fnOS 桌面外壳绘制，尺寸与能否拖拽缩放都由外壳决定 —— 官方文档里连
"窗口"这个概念都只以 `iframe`/`url` 两种打开方式出现。想让应用默认更大，只有 `type=url`
（浏览器整屏打开），代价是失去桌面内嵌。

所以本应用的做法是**在任意窗口尺寸下都好用**，再给一个一键放大：

- **⛶ 全屏按钮**：软件中心页面右上角（⚙ 下方）和设置面板顶栏各有一个。调用标准
  `requestFullscreen()`；页面跑在桌面外壳的**同源** iframe 里，请求会作用到顶层文档，
  也就是**整个窗口**变全屏。老 WebKit 内核回退 `webkitRequestFullscreen`，都不支持才提示
  用桌面窗口自己的最大化按钮（不去猜宿主桥的方法名）。
  实现见 `app/ui/assets/fullscreen.js`，测试见 `service/ui_test.go`。
- **设置面板自适应**：`max-height: 760px` 时收紧留白、`max-width: 560px` 隐藏次要信息、
  `max-width: 420px` 单列化；所有 flex 项都有 `min-width: 0` 约束，任何宽度都不会横向溢出
  （用无头浏览器实测：900×560 与 504×605 下 `docScrollWidth == 视口宽度`）。
- 本机想看效果（无头浏览器截图，需要真在跑的服务）：

  ```bash
  python3 tools/preview_ui.py 18101        # 输出 tools/ui-preview/*.png
  ```



## 图标

图标用的是 **ui.com 官方标志**：

| 用途 | 文件 | 尺寸 |
| --- | --- | --- |
| 包图标 | `ICON.PNG` / `ICON_256.PNG` | 64×64 / 256×256 |
| 桌面入口图标 | `app/ui/images/icon_{64,256}.png` | 64×64 / 256×256（入口配置的 `{0}` 替换） |
| 页面 favicon | `app/ui/assets/icon.png` | 256×256 |

**图源**：`https://ui.com/microsite/apple-touch-icon.png`（官方 256×256 PNG，清晰）。
`https://ui.com/microsite/favicon.ico?v=2` 里最大的图只有 48×48，放大会糊，所以只作备选
（`tools/ui-favicon.ico`，脚本也能直接读 ICO）。

**每次构建都会重新生成**（`deploy/build.*` 会先跑 `tools/make_icon.py`），脚本是纯标准库的
PNG/ICO 解码 + 三角滤波缩放 + PNG 编码，不需要 Pillow 或 ImageMagick。按 fnOS 图标规范
（"主体使用圆角矩形，不要直角满铺"）默认给四角加 21% 的抗锯齿圆角；想用官方原样的直角方图：

```bash
python3 tools/make_icon.py . --radius 0        # 直角
python3 tools/make_icon.py . --radius 0.18     # 换个圆角比例
```

换图标：把新图放到 `tools/` 下，然后
`python3 tools/make_icon.py . --source tools/新图.png`。

## 运行模型（为什么是统一网关 + Unix Socket + 静态二进制）

- 软件中心需要长轮询、流式响应对长超时，还要复用 NAS 登录态 ⇒ 按官方建议选**统一网关**
  （`index.cgi` 每次请求起进程，官方明确不适合长耗时/常驻服务）；
- `app/ui/config` 里注册 `gatewayPrefix=/app/unificenter`、`gatewaySocket=app.sock`；
  官方要求 Socket 放在应用 `target` 目录下，所以服务监听 **`$TRIM_APPDEST/app.sock`**
  （`cmd/main` 里启动，并等待 Socket 真正出现才算启动成功，退出码按 0/1/3 约定）；
- 服务端**静态编译**：`CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build -trimpath -ldflags '-s -w'`，
  目标机不需要任何解释器或动态库；因此 `manifest` 里 `platform` 必须按架构声明
  （含特定架构二进制不能用 `all`），也不声明 `install_dep_apps`；
- 网关注入的可信身份头 `X-Trim-Userid` / `X-Trim-Isadmin` / `X-Trim-Username` 是本应用
  判定权限的唯一依据：**明文 AdminToken 只对管理员可见**，普通用户与局域网直连只能替换、看不到原值
  （失败关闭：拿不到网关身份就当非管理员）；
- 路径全部走 `TRIM_APPDEST` / `TRIM_PKGETC` / `TRIM_PKGVAR` / `TRIM_PKGTMP`，不硬编码卷与安装目录；
- `manifest` 里 `checkport=false`（不监听固定端口），`ctl_stop=true`（应用中心可启动/停止）。

## 接口

本应用自己的接口（网关前缀 `/app/unificenter` 下，`…/ctl/…`）：

| 路径 | 方法 | 说明 |
| --- | --- | --- |
| `/ctl/status` | GET | 目标可达性、延迟、软件中心标题、设备与版本、令牌状态、Socket 状态、本地资源 |
| `/ctl/settings` | GET | 目标地址、`token_set`、`token_hint`、`token_visible`、`can_edit`；**明文 `token` 只对管理员** |
| `/ctl/settings` | POST | `{target?, token?}`（仅管理员）；`token: ""` 表示清除令牌 |
| `/ctl/token` | GET | 取明文 AdminToken（**仅管理员**，供设置面板的"显示"按钮用） |
| `/ctl/logs` | GET | 服务日志尾部 200 行 |
| `/ctl/panel.html` | GET | 设置面板页面 |
| `/healthz` | GET | 健康检查（版本、目标、运行时长、Socket、本地资源） |

软中心与转发：

| 路径 | 说明 |
| --- | --- |
| `/`（可写 `/index.html`、`/control`） | 软件中心页面（本地化 + 令牌注入后返回） |
| `/api/…` | 转发到路由器软件中心的接口（超时 1 小时，适配长轮询） |
| `/github/api/…`、`/github/raw/…` | 转发页面里对 GitHub 的直连（**不带** Authorization） |
| `/view/…` | 兼容入口（页面 + 接口转发，对应原插件的老路径） |
| `/assets/…`、`/images/…` | 本地静态资源 |

### 转发到路由器时的 Authorization

`/api/…` 转发前会过一遍 `ResolveAuthorization`：

| 收到的 Authorization | 转发用的值 |
| --- | --- |
| 客户端自己带的真令牌（页面里手输、入口注入） | 原样透传，绝不覆盖 |
| 控制台电脑端那一跳的占位值 `Bearer console-loopback` | 本应用保存的令牌 |
| 完全没带 | 本应用保存的令牌；没保存就不带（软件中心照实报 401，日志里写清该去哪填） |

## 安全

- 应用的运行身份是 fnOS 包用户（`config/privilege` 里 `run-as=package`），不是 root；
- 这个界面能在路由器上装包、重启服务 ⇒ 请给 AdminToken 用强随机值，并且**不要**给路由器的
  软件中心端口做公网映射（本方案的价值就是不需要）；
- AdminToken 存在 `$TRIM_PKGETC/router-token`（0600），配置目录 0700，写入走"临时文件 + rename"；
- 明文令牌的可见性：**只有经网关登录的管理员**（`X-Trim-Isadmin: true`）能通过
  `/ctl/settings` 与 `/ctl/token` 读到；其它入口只回 `token_hint`（例如 `a1b2…z9`），
  响应体里不会出现明文；页面注入不受影响（那是服务自己读文件写进页面 localStorage 的）；
- 修改地址/令牌（`POST /ctl/settings`）同样只允许管理员，其他人 403；
- 路径参数一律当不可信输入：拒绝 `..`、解码后再判、解析软链后最终路径必须落在 UI 目录内，
  只提供白名单文件；
- 服务只监听 Unix Socket（`/var/apps/unificenter/target/app.sock`），
  唯一例外是调试用的 `UNIFICENTER_DEV_PORT`（默认关闭）。

## 测试

```bash
cd service && go test ./...          # 单元 + HTTP 层 + 构建产物验证
go vet ./...
```

覆盖：目标地址规整与非法值、设置/令牌读写与权限、转发时 Authorization 的四种取值、
假路由器探测（可达/带令牌/错令牌/不可达，且必须是只读 GET）、页面重写（CDN 本地化、
GitHub 改走 NAS、令牌注入、令牌里的引号必须被 JSON 转义）、网关身份（管理员/普通用户/无网关头/
大小写容错/伪造管理员头无效）、URL 拼接（空 path 不能补尾斜杠，否则上游 301）、
HTTP 行为（`/healthz`、`/ctl/*`、静态资源、路径穿越拒绝、未知接口 404、页面前缀本地化、
`/api` 转发的 401/透传/占位值替换/502）、`/github` 转发不带 Authorization，以及
**构建产物**（x86/arm 包的 ELF 架构与静态链接、包内可执行位、manifest 的 platform 与 checksum）。

端到端冒烟（在 Linux 或 WSL 里跑**真正的二进制**，含 Unix Socket）：

```bash
bash tools/smoke.sh dist/unificenter-1.0.0-x86.fpk
```

它会解包 .fpk、按安装后的布局摊平到 `target/`、起一个假路由器，然后：跑 `selftest`、
用 `cmd/main start` 拉起服务、**经 Unix Socket** 打全部接口（页面本地化与令牌注入、
`/api` 转发带令牌、管理员能看明文/能改设置、普通用户只读且改设置 403、路径穿越被拒），
验证 `cmd/main status/stop/start` 的退出码与 Socket 清理，最后**再按第二种布局**
（`TRIM_APPDEST` 就是应用根目录、二进制在 `APP/bin/`、Socket 在 `APP/app.sock`）跑一遍
start/healthz。共 51 项检查。

本机调试（不装应用也能跑起来看界面；调试口没有网关头，因此一律按"非管理员"处理，
看不到明文令牌、也改不了设置 —— 与线上一致，避免调试口成为后门）：

```bash
UNIFICENTER_DEV_PORT=18101 TRIM_PKGETC=./.dev/config ./app/bin/unifi-center serve
# 浏览器打开 http://127.0.0.1:18101/           页面（软件中心）
#            http://127.0.0.1:18101/ctl/panel.html 设置面板
```

## 与原插件（xiaomi-router-center）的对照

| 能力 | 小米智能存储插件（原） | 本应用（fnOS） |
| --- | --- | --- |
| 入口 | 插件页 `/plugin/<用户>/rtrcenter/` | 统一网关 `/app/unificenter/` |
| 传输 | nginx → 127.0.0.1:18101 | fnOS 网关 → `$TRIM_APPDEST/app.sock` |
| 服务端 | Python + 系统 python3 依赖 | **静态 Go 二进制，零依赖** |
| 入口注册 | 安装脚本渲染 nginx conf + 注册表 | `app/ui/config` 的 `gatewayPrefix`/`gatewaySocket` |
| 身份判据 | 客户端证书 `CN=nas.<用户号>.…` | 网关头 `X-Trim-Userid`/`X-Trim-Isadmin`/`X-Trim-Username` |
| 明文令牌 | 只给"设备所有者的小米客户端" | 只给"已登录的 fnOS 管理员" |
| 配置位置 | `/data/plugin/router-center/{settings.json,router-token}` | `$TRIM_PKGETC/{settings.json,router-token}` |
| 生命周期 | systemd 单元 + plugin.sh | `cmd/main`（start/stop/status）+ 安装/配置/卸载钩子 |
| 页面本地化 / GitHub 转发 / 令牌注入 / Authorization 规则 | 有 | **原样保留** |
| 打包 | 手写安装脚本 | `.fpk`（`deploy/build.*`，或官方 `fnpack build -d .`） |

`cmd/upgrade_callback` 与 `unifi-center migrate-legacy` 提供了一次**幂等的旧数据迁移**：若本应用
还没有配置、而 `/data/plugin/router-center/` 有，就复制过来（只读旧目录，不删旧数据）。

## 已知限制

- 路由器上各应用自己的 Web 面板（xwall、webssh 等，各自监听不同端口）默认打不开，
  需要的话可以再加"通用端口反代"；
- 软件中心前端依赖 Vue / Tailwind / Lucide，本应用已带本地副本（`app/ui/assets/`），
  若软件中心改版换了 CDN 地址，日志里会出现"页面里仍有外部脚本未本地化"，把新地址加进
  `engine.go` 的 `assetRoutes` 并补一份本地副本即可；
- WebSocket 未纳入反代（软件中心本身是 REST + 长轮询，长轮询已按大超时处理）；
- 两个架构的包是分开的：设备是 ARM 就得装 arm 包（包里是特定架构的二进制）。

## 持续集成与发布（GitHub Actions）

工作流：`.github/workflows/build.yml`

| 触发 | 做什么 |
| --- | --- |
| 任何 push / PR | `go vet` + `go test` + 构建两个架构的 `.fpk` + `tools/verify_fpk.py` 校验 + 冒烟测试；产物上传为 artifact（**不发布**） |
| push 到 `main`，且**最新提交标题里含 `[release]`** | 在上面基础上，额外创建 GitHub Release，并把两个 `.fpk` 作为附件 |
| 手动 `workflow_dispatch`（勾 `force_release`） | 同样发布 |

发布标记就是提交标题里的 **`[release]`**（`RELEASE_MARKER` 可改），例如：

```bash
git commit -m "feat: xxx [release]"
git push origin main
```

### 版本与 tag 规则

- 版本号唯一来源是 **`manifest` 里的 `version`**（`deploy/build.*` 也用它命名产物）；
- tag 形如 `v1.0.0`；**同一版本要再发一次**会自动递增成 `v1.0.0-r2`、`-r3`…（与 fnOS 社区惯例一致），
  不会覆盖已有 Release；
- 所以"发新版本"= 改 `manifest` 的 `version` 再带 `[release]` 提交；"同版本重发"= 直接带 `[release]` 提交。

### 发布前会卡住的两类问题（都在 CI 里拦下）

1. `tools/verify_fpk.py` 校验**真正要发出去的那个 .fpk**：顶层结构（`app.tgz` 里不能多套一层
   `app/`，否则装到设备上会"找不到可执行文件"）、`checksum` 与 `app.tgz` 的 md5 一致、
   `bin/unifi-center` 有可执行位且是**静态链接**的对应架构 ELF、图标尺寸与大小、向导 JSON 合法；
2. `tools/smoke.sh` 在 CI 里**真跑一遍二进制**：解包 → 按安装后布局摊平 → 起假路由器 →
   经 Unix Socket 打全部接口（权限、令牌可见性、页面本地化、转发、路径穿越）→ 校验
   `cmd/main start/status/stop` 的退出码。
