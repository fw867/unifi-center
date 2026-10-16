# legacy/python —— 最初的 Python 实现（仅作参考，不参与构建）

当前应用的服务端是 **Go 静态二进制**（`service/`），这里的文件是最早的 Python 版本，
保留下来是为了对照阅读：

| 文件 | 说明 |
| --- | --- |
| `server/server.py` | Python 版路由与 Unix Socket 监听 |
| `server/engine.py` | Python 版配置/令牌/页面重写/探测 |
| `server/seed_settings.py` | Python 版向导落盘与旧数据迁移 |
| `tests/test_engine.py` | Python 版测试（65 个用例） |
| `smoke.py` | Python 版端到端冒烟 |

## 为什么不再用它

- 目标机（fnOS）系统**不带 Python**，用它就得在 `manifest` 里声明 `install_dep_apps=python312`，
  装应用时会连带装一个 Python 运行时应用；
- 改成 Go 之后，`CGO_ENABLED=0` 编译出的静态二进制解包即跑，**零运行时依赖**。

Go 版与 Python 版行为一致：同样的接口路径、同样的 Authorization 三条规则、同样的
"明文令牌只给管理员"边界，测试用例也逐条对应（见 `service/main_test.go`）。

## 想跑这版 Python 实现

它按旧的目录结构（`app/server/unifi_center/`）写路径，直接跑需要把文件放回那个位置：

```bash
mkdir -p /tmp/unifi-python/app/server
cp -r server /tmp/unifi-python/app/server/unifi_center
cd /tmp/unifi-python
TRIM_PKGETC=/tmp/unifi-python/etc UI_DIR=<项目>/app/ui \
  python3 app/server/unifi_center/server.py      # 需要 UNIFICENTER_DEV_PORT 时另加环境变量
```

测试同理（`sys.path` 指向 `app/server/unifi_center`）：

```bash
python3 -m unittest discover -s tests -v
```

只是想验证当前应用的话，请用 Go 版：`cd service && go test ./...` 与
`bash tools/smoke.sh dist/unificenter-1.0.0-x86.fpk`。
