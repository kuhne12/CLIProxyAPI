# BPS 授权与推理插件

复用 [bps-local-public](https://github.com/sunboyzoey/bps-local-public) 的浏览器授权、设备码授权和
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

默认使用普通浏览器 OAuth 登录：命令打印登录链接，你在任意电脑的浏览器完成登录和授权，
再将地址栏中的完整回调链接粘贴回终端，CPA 自动交换并保存凭据，**无需设备授权码**。
最长等待 15 分钟；Ctrl+C 取消。省略 `--no-browser` 会尝试打开本机浏览器。

BPS 注册的回调地址固定为 `https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback`，
不能替换成 CPA 地址。回调页面即使显示错误，也可复制带 `code` 和 `state` 的完整地址。
这是手动回传链接，无需在服务器安装浏览器。不要将回调链接发送给他人。

设备码仍可显式选择：

```sh
./bin/cli-proxy-api --config config.yaml --bps-login --bps-login-method device --no-browser
```

导入不会修改源文件。支持原版 bps-local 扁平文件，以及显式带 BPS `client_id` 的
`tokens` 嵌套文件；必须能够提取账号和绝对过期时间。拒绝混入 Codex 凭据。

结果按独立的 `type: bps` 保存，文件名为稳定的 `bps-<身份哈希>.json`。
文件存储的新凭据权限为 `0600`；同一身份再次导入会更新目标文件，保留源凭据的
`prefix`、`proxy_url`、`disabled` 等设置。命令完成后显示保存路径。

## 管理 API

先配置 `management.secret-key`，以下请求使用管理密钥。

启动普通浏览器授权（省略 `method` 也默认使用浏览器模式）：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=bps&method=browser'
```

返回 `url`、`state`、`interval`（秒）和 `expires_at`，不返回 `user_code`。
打开 `url` 完成登录，复制完整回调链接，通过 CPA 已有接口提交：

```sh
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"provider":"bps","redirect_url":"PASTE_FULL_CALLBACK_URL_HERE"}' \
  'http://127.0.0.1:8317/v8/management/oauth/callback'
```

然后用启动时返回的 `state` 查询状态，驱动 Token 交换和凭据保存：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/status?state=RETURNED_STATE'
```

`wait` 表示等待授权，`ok` 表示凭据已保存，`error` 表示需要重新发起。
轮询请求驱动授权过程，没有后台轮询器。普通浏览器授权使用独立 PKCE 和 state；
交换失败不会重试同一授权码。选择 `method=device` 时才返回设备码，并限制轮询频率、处理 `slow_down`。
重复或并发轮询不会重复交换授权码、重复保存；响应不包含 Token、PKCE verifier 或私有设备 ID。

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

## 使用这份授权进行推理

从插件 **0.3.0** 起，需要同时升级到本仓库对应的 CPA 构建。插件提供授权与模型目录，
CPA 的 BPS 执行器直接复用上游请求/工具转换和流式响应适配，通过 WebSocket 调用 BPS。
现有 `type: bps` 文件无需重新登录、改类型或启动额外桥接进程。

可用路由为 `bps-gpt-6-astra`、`bps-gpt-6-sol`、`bps-gpt-6.2-astra`、
`bps-gpt-6.2-sol`。`bps-` 前缀确保选择 BPS 凭据；上游收到原始模型 ID。
这是客户端可选目录，账号权限和限流仍由 BPS 决定。

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"bps-gpt-6-astra","messages":[{"role":"user","content":"只回复 OK"}],"stream":false,"reasoning_effort":"low"}'
```

同时支持 `/v1/responses`，以及流式和非流式响应。可在管理页验证这份凭据，或调用：

```sh
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"name":"bps-IDENTITY_HASH.json","model":"bps-gpt-6-astra"}' \
  'http://127.0.0.1:8317/v8/management/credentials/verify'
```

## 当前范围

- 已实现授权、导入、统一保存、手动刷新、模型注册、原生推理和用量统计；不自动定时刷新。
- 复用上游的文本、客户端工具和结构化输出适配；图片使用 HTTPS URL。
  不支持内联图片上传、上游托管搜索、Excel 工作簿操作、`previous_response_id` 或 token 计数接口。
- 每账号最多 4 个并发请求和 64 个等待请求，取消请求会关闭上游连接并释放槽位。
  网络断开后不自动重放已接受的生成；上游限流明确返回 429。
- BPS 上游会添加产品系统上下文，简单请求也可能产生约 22k 输入 Token；用量按实际返回值统计。
- 使用当前 CPA 构建；管理前端登录仍使用 CLI/API，浏览器登录采用手动回传链接。

上游来源和许可证见 [NOTICE](NOTICE)、[LICENSE.upstream](LICENSE.upstream)。
详细验证命令见 [英文说明](README.md)。
