# BPS 授权插件

复用 [bps-local-public](https://github.com/sunboyzoey/bps-local-public) 的设备码授权和
Token 刷新协议，接入 CPA 已有的插件加载、CLI、凭据存储、代理配置、v8 管理会话和手动刷新。

## 构建与配置

在 **CPA 仓库根目录**执行，需要 Go 1.26+ 和 C 编译器。插件复用根 Go module。
CPA 和插件都要开启 CGO，且操作系统、CPU 架构必须一致。

```sh
mkdir -p bin plugins
CGO_ENABLED=1 go build -o bin/cli-proxy-api ./cmd/server
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/bps.dylib ./examples/plugin/bps/go
```

Linux/FreeBSD 使用 `bps.so`，Windows 使用 `bps.dll`。Apple Silicon 上如 Go 环境
默认目标是 `amd64`，请在两条构建命令前都加 `GOARCH=arm64`。生成的 `.h` 不需要部署。
保留 `bps` 文件名，与配置中的插件 ID 对应。

把下面配置合并到实际使用的 v8 配置文件中，并调整目录：

```yaml
config-version: 8
oauth:
  auth-dir: "./auths"
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    bps:
      enabled: true
```

登录使用 CPA 的 `requests.proxy-url`，刷新还支持凭据内的 `proxy_url` 覆盖值。
代理传输复用 CPA 实现，支持 `direct`、HTTP(S)、SOCKS。

## 登录或导入

```sh
./bin/cli-proxy-api --config config.yaml --bps-login --no-browser
./bin/cli-proxy-api --config config.yaml --bps-import /absolute/path/oauth.json
```

登录会立即打印授权地址和设备码。打开地址、输入设备码并确认授权后，命令自动保存凭据，
最长等待 15 分钟；Ctrl+C 取消。省略 `--no-browser` 会自动打开浏览器。
这仍然是设备码授权，尚未实现独立的浏览器回调登录模式。

导入不会修改源文件。支持原版 bps-local 扁平文件，以及显式带 BPS `client_id` 的
`tokens` 嵌套文件；必须能够提取账号和绝对过期时间。拒绝混入 Codex 凭据。

结果按独立的 `type: bps` 保存，文件名为稳定的 `bps-<身份哈希>.json`。
文件存储的新凭据权限为 `0600`；同一身份再次导入会更新目标文件，保留源凭据的
`prefix`、`proxy_url`、`disabled` 等设置。命令完成后显示保存路径。

## 管理 API

先配置 `management.secret-key`，以下请求使用管理密钥。

启动设备码授权：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=bps&method=device'
```

返回 `url`、`user_code`、`state`、`interval`（秒）和 `expires_at`。
展示地址和设备码，并按返回的间隔轮询：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/status?state=RETURNED_STATE'
```

`wait` 表示等待授权，`ok` 表示凭据已保存，`error` 表示需要重新发起。
轮询请求驱动授权过程，没有后台轮询器。插件会限制实际请求频率，并处理上游 `slow_down`。
重复或并发轮询不会重复交换授权码、重复保存；响应不包含 Token、PKCE 或私有设备 ID。

取消尚未提交保存的授权：

```sh
curl -X DELETE -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/session?state=RETURNED_STATE'
```

`cancelled: true` 保证后续返回的授权结果不会写入凭据；已进入保存阶段则返回 false，
并继续完成保存。重启或卸载插件会使尚未完成的会话失效。

列出凭据，并用列表中的文件名手动刷新：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/credentials'
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"name":"bps-IDENTITY_HASH.json"}' \
  'http://127.0.0.1:8317/v8/management/credentials/refresh'
```

刷新保持账号身份、文件名和设置，并由 CPA 保存轮换后的 Token。
Token 交换失败不自动重试；明确的 `invalid_grant` 需要重新登录。
同一授权应只由一个进程管理刷新，多个独立进程之间没有刷新轮换协调。

## 当前范围

- 已实现授权、导入、统一保存和手动刷新；没有注册模型、原生 BPS 推理执行器或自动刷新。
- 保存的扁平凭据可交给现有 `bps-local --auth`，推理继续使用单独配置的本地桥接服务及
  CPA Codex 渠道。不要把 BPS 文件改成 Codex OAuth 类型。
- v8 设备码响应需要本次修改后的 CPA。现有管理前端尚未添加 BPS 登录按钮，可使用 CLI/API。
- 自动化测试使用模拟 OAuth 和合成凭据；真实账号授权与模型生成权限尚需实际账号验证。
  授权成功本身不代表具有生成权限。

上游来源和许可证见 [NOTICE](NOTICE)、[LICENSE.upstream](LICENSE.upstream)。
详细验证命令见 [英文说明](README.md)。
