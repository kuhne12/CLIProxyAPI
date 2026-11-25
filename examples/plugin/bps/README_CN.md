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

可用路由为 `gpt-6-astra`、`gpt-6-sol`、`gpt-6.2-astra`、
`gpt-6.2-sol`。从插件 **0.3.1** 起，模型 ID 与上游 GPT 保持一致，不添加渠道前缀。
如果其他渠道提供同名模型，CPA 会按现有凭据路由策略选择；单独验证 BPS 时应指定这份凭据。
这是客户端可选目录，账号权限和限流仍由 BPS 决定。

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-astra","messages":[{"role":"user","content":"只回复 OK"}],"stream":false,"reasoning_effort":"low"}'
```

同时支持 `/v1/responses`，以及流式和非流式响应。可在管理页验证这份凭据，或调用：

```sh
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"name":"bps-IDENTITY_HASH.json","model":"gpt-6-astra"}' \
  'http://127.0.0.1:8317/v8/management/credentials/verify'
```

### 响应格式兼容修复（0.5.2）

配套 CPA 同时兼容单行和带缩进的多行 WebSocket JSON。正文中的换行保持不变，
上游模型无权限、额度不足等错误可正常分类，不再被误报为不支持的响应或工具调用。
此修复需要同时升级 CPA；模型出现在可选目录中不代表当前账号拥有调用权限。

## 查询额度与余额

从插件 **0.4.0** 起，BPS 认证文件会显示 CPA 已有的额度查询入口。查询使用与 Codex
相同的账户用量接口；BPS 官方页面也将用量和积分入口指向 Codex 的账户用量页。
支持显示套餐、额度剩余比例、重置时间，以及上游明确返回的积分余额。
余额为空或未返回时保持未知，不显示为 0，也不把积分换算为金额。

在认证文件页面点击 BPS 文件的额度查询按钮，或调用：

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/plugins/bps/quota?auth_index=YOUR_AUTH_INDEX'
```

`auth_index` 可从 `/v8/management/credentials` 获取。查询复用 CPA 的请求取消和
凭据/全局代理设置，不发起模型推理，不消耗生成 Token；不支持手动重置上游额度。

### 预计剩余美元额度（0.5.1）

在 BPS 插件配置中增加 `quota-model-prices` 后，原有查询入口会同时显示预计剩余和
预计总美元额度。价格单位为 **USD / 百万 Token**，必须由管理员填写；插件不内置或
自动更新价格，也不会将上游积分数当作美元余额。以下为配置格式示例，实际价格应以
你的计价来源为准：

```yaml
plugins:
  configs:
    bps:
      enabled: true
      cache-creation-as-input: true
      quota-model-prices:
        gpt-6-astra:
          input: 10
          output: 50
          cache-read: 1
          cache-write: 12.5
          priority-multiplier: 2
          flex-multiplier: 0.5
          long-context-threshold: 272000
          long-context-input-multiplier: 2
          long-context-output-multiplier: 1.5
```

参考 Sub2API 的“窗口消费金额 ÷ 已用比例”方法，但以**两次查询之间的增量**校准，
避免刚安装时把部分消费除以整个窗口的已用比例：

- 采样费用 = 普通输入 × 输入价 + 缓存读取 × 读取价 + 缓存创建 × 创建价 + 输出 × 输出价。
  上述 Token 数均除以一百万；输出已含推理 Token，不重复相加。启用
  `cache-creation-as-input` 后，创建缓存按输入价计算，输入总数不重复累加。
- 预计总额度 = 两次查询间的已定价消费 × 100 ÷ 已用百分比增量。
- 预计剩余额度 = 预计总额度 × 当前剩余比例。不同窗口单独计算，不相加。

首次查询建立基准；之后经正常使用、已用比例累计增加至少 **1 个百分点**，再次查询
才会显示金额。样本不足显示采样状态，不显示虚假的 `$0`。查询仅调用用量接口，不额外
生成请求“刷”额度。采样按认证文件隔离，只统计 CPA 收到的 BPS 用量；重复执行 ID 去重。

需要为实际响应模型配置准确价格；缺少对应模型、缓存单价或服务档倍率，以及失败请求，
会重置当前采样区间，但保留已完成的有效估算。窗口重置、已用比例下降、套餐或价格改变，
才会使已完成的估算失效。
`priority-multiplier` 和 `flex-multiplier` 仅用于对应服务档，缺少时不会猜测；
超过 `long-context-threshold` 后，对输入及缓存应用输入倍率，对输出应用输出倍率。
省略长上下文字段表示所配置单价不分上下文长度。

这是**按配置价格折算的预计 API 价值**，不是官方查询到的美元余额，也不能兑现。
同账号在其他客户端的消费、模型组合变化、百分比取整及用量上报延迟都会影响估计。
不自动应用 Sub2API 的用户折扣、分组倍率或账户余额。仅针对账户主额度窗口，
不推算 Code review 等独立限额。为避免将服务中断期间的未知消费混入样本，
0.5.3 起将已完成的校准原子保存到 `plugins/bps-quota-estimates.json`，文件权限为 0600，
仅保存额度窗口、数字估值及认证标识哈希，不保存 Token、邮箱或对话。请保留 plugins 目录挂载。
重启后的首次查询核对套餐和额度窗口，同时开始新的采样区间，已完成的估算继续显示。
不完整的采样仍只保存在内存；文件读写失败时退回重新采样或内存估算。

## Base64 图片支持（0.5.3）

复用 bps-local-public 的 ImageRelay，将 data:image/base64 图片转换为当前 CPA
服务器上的临时 HTTPS 地址后交给 BPS。配置示例：

```yaml
plugins:
  configs:
    bps:
      enabled: true
      image-relay-origin: https://cpa.example.com
```

域名必须指向当前 CPA，反向代理须放行 `/api/bps-images/` 的 GET、HEAD 请求，不能额外
要求登录。仅经过 CPA API 认证的生成请求能创建图片；下载地址使用不可猜测的签名，
十分钟到期并自动删除。文件使用私有权限，不公开磁盘目录，也不使用第三方图床。
只接受 PNG、JPEG、GIF、WebP，单图不超过 20 MiB / 6400 万像素；单请求不超过
32 MiB / 128 张；整个服务最多保存 1 GiB / 512 张图片，并限制同时下载数。

Chat Completions、Responses 的流式和非流式请求均适用，支持可识别的工具输出图片。
已有 HTTPS 图片无需中转；留空配置继续只接受 HTTPS。修改域名只影响新链接，
旧链接需要原反向代理继续工作至到期。图片转换在最终 payload 配置前完成，不覆盖用户规则。

## 创建缓存按普通输入计费（0.5.0）

同时升级 CPA 与插件到对应版本后，可在 BPS 插件配置中启用此开关，默认关闭：

```yaml
plugins:
  configs:
    bps:
      enabled: true
      cache-creation-as-input: true
```

开启后，执行器在向下游返回用量前，将创建缓存计数归零并移除其 TTL 明细；
Chat Completions、Responses 的流式与非流式请求均适用。总输入已经包含创建缓存，
因此不会重复相加。缓存读取、输出、推理及总 Token 数保持不变。
例如总输入 1,000，其中缓存读取 100、创建缓存 200，下游收到的总输入仍是 1,000，
缓存读取 100、创建缓存 0；按扣除缓存读取方式计费的网关会计算 900 个普通输入 Token。

CPA 内部用量记录仍保留上游原始分类，便于核对。此设置仅改变下游用量分类，
不会禁用上游缓存、移除 BPS 产品上下文、改变上游费用、设置下游单价或修改历史账单。
设置对新请求生效，进行中的流式请求保持开始时的设置；关闭后恢复原始分类。
模型 ID 不变。

## 当前范围

- 已实现授权、导入、统一保存、手动刷新、模型注册、原生推理、用量统计和账户额度查询；不自动定时刷新。
- 复用上游的文本、客户端工具和结构化输出适配；图片使用 HTTPS URL。
  不支持内联图片上传、上游托管搜索、Excel 工作簿操作、`previous_response_id` 或 token 计数接口。
- 每账号最多 4 个并发请求和 64 个等待请求，取消请求会关闭上游连接并释放槽位。
  网络断开后不自动重放已接受的生成；上游限流明确返回 429。
- BPS 上游会添加产品系统上下文，简单请求也可能产生约 22k 输入 Token；用量按实际返回值统计。
- 使用当前 CPA 构建；管理前端登录仍使用 CLI/API，浏览器登录采用手动回传链接。

上游来源和许可证见 [NOTICE](NOTICE)、[LICENSE.upstream](LICENSE.upstream)。
详细验证命令见 [英文说明](README.md)。
