# BPS 授权与推理插件

复用 [bps-local-public](https://github.com/sunboyzoey/bps-local-public) 的浏览器授权、设备码授权和
Token 刷新协议，接入 CPA 已有的插件加载、CLI、凭据存储、代理配置、v8 管理会话和手动刷新。

## 兼容性修复（0.5.4）

CPA 和 BPS 动态库需要一起升级。本补丁保留 Responses 请求显式传入的
`context_management`，避免通用 Codex 转换删除它后退回默认压缩阈值。
用户 payload 覆盖与过滤仍在全部 BPS 转换完成后最后执行。

生成发送前的 WebSocket 握手按上游策略区分瞬时和永久错误，最多尝试 3 次，
退避为 250/500 毫秒，支持取消。永久错误或重试耗尽后停止 CPA 外层重试，
不惩罚凭据。明确的 HTTP 拒绝由 CPA 原有调度处理，并保留 `Retry-After`
秒数或 HTTP 日期；原生插件错误也将该等待提示传给调度接口。
握手预算服从调用方 context，不移植独立 CLI 的固定握手或建连后超时。
生成请求已经发送后，仍禁止自动重放。

补齐 `web_search_2025_08_26_preview` 托管工具别名：自动模式下移除不支持的
声明并向模型说明限制；明确强制托管搜索时继续拒绝，不静默假装完成搜索。
正常 GPT 模型 ID 保持不变。

本补丁不改变上游产品提示词、账号权限或美元余额估算语义。账户启动节流、
安装实例 task 命名空间隔离另行安排后续补丁。

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

从 **0.5.7** 起，插件使用各账号的授权查询
`/basispoints/api/responses/access?include_models=true`，并从上游
`model_catalog` 同步模型 ID、名称和推理档位，不再维护固定模型列表。
凭据注册时发现模型，独立 CPA 模式每五分钟自动刷新；一分钟的按凭据内存缓存合并重复注册查询。
推理请求不会等待目录刷新。网络错误、429/5xx 和格式异常会保留当前进程最近一次成功注册的目录；
首次启动查询失败时不发布模型，后续刷新成功后恢复。
401/403、`allowed:false` 或明确的空目录会移除该凭据的模型，
`restricted_models` 中的模型不会注册。故障时不会退回固定模型列表。
设置面板的模型建议也从已注册凭据读取。
从插件 **0.3.1** 起，模型 ID 与上游 GPT 保持一致，不添加渠道前缀。
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

### 目录外原生工具的纠错续推

已对照上游 bps-local-public 的
[c66fb410](https://github.com/sunboyzoey/bps-local-public/tree/c66fb41006baf7ede70af69a3e2f81af6a6c246b)。
保留上游已有的运输封装修复、目录内工具直呼和原生计划工具适配；
上游对目录外原生工具仍直接报错，没有针对这种情况的纠错生成。

CPA 增加两层处理：首先在历史末尾追加 developer 提醒，明确当前客户端工具目录，
要求工具发现和执行使用既定的转发协议。若 BPS 仍在完整响应末尾返回唯一一个
目录外原生函数调用，CPA 最多续推一次：保留真实调用，补入
`client_tool_unavailable` 错误结果，明确说明工具没有执行，请模型重新选择。
不会把未知工具猜测映射成客户端工具，也不会伪造成功结果。续推使用同一账号、模型、
task 和 turn，生成新的运输请求 ID，在用户 payload 规则执行前递增 agent iteration。

仅在完整响应、尚未交付工具时允许纠错。混合或多个工具调用、参数异常、首次响应含压缩、
结构化输出、空客户端目录，以及 payload 规则修改 input 时均不启用。网络断流和上游错误
不会触发纠错；第二次仍不合法则正常失败，不继续生成。纠错失败（包括续推连接被 HTTP
拒绝）会阻止 CPA 外层换账号重试或重放原请求。握手阶段原有的重试预算另算。

文本继续增量交付；纠错成功后保持同一个客户端响应 ID 和连续事件序号，拒绝重复输出或
调用 ID。两次生成的已知 Token 用量合并上报，纠错失败也保留已收到的用量。
每次生成都在全部内部处理完成后最后执行一次用户 payload 规则。纠错历史产生的压缩
检查点不会被用于确认原始客户端历史。纠错会增加一次生成的延迟和用量，不能保证
模型第二次一定选择正确工具。

原有校验日志保留被拒绝的原生工具信息；最终 `BPS request timing` 日志增加
`tool_recovery_attempts`、`tool_recovery_outcome`（`completed` / `failed` / `cancelled`），
以及已构造纠错请求时的 `tool_recovery_upstream_bytes`。新增字段只含计数和固定状态，
不记录提示词、参数、工具结果或凭据。实现位于 CPA 执行器和桥接层，单独替换授权
动态库不会安装此修复。回归测试使用本地模拟 WebSocket 响应，不发起生产生成请求。

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

## 设置面板（0.5.6）

在 CPA 管理中心的插件菜单打开 **BPS 设置**。可以调整缓存写入的计量方式、图片中转的 HTTPS 地址，以及每个模型的单价和服务档、长上下文倍率。页面可复用同一实例的已记住登录；也可在页面输入管理密钥。请求设置和价格分别保存，使用现有 v8 配置接口持久化并热加载。如果有其他人同时修改配置，请重新读取后再编辑。

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

## 压缩与工具响应诊断日志

日志位于 **CPA 主程序的 BPS 原生执行器**。需要使用包含此修改的 CPA 构建；仅替换
BPS 授权插件的共享库不会启用这些日志。无需新增插件配置，也无需开启完整请求体日志。

正常 info 级别下，现有 `BPS request timing` 会额外记录：

- 工具目录：`tool_catalog_count`、完整合并声明的 `tool_catalog_hash`，以及最多 64 个
  排序后的工具名。目录哈希包含描述中的 exec helper 声明；描述正文不写日志。`scope_hash` 用于关联同一账号、调用方和会话作用域。
- 历史裁剪：`history_status` 区分 `no_checkpoint`、`input_not_array`、
  `checkpoint_not_found`、`invalid_checkpoint`、`checkpoint_without_prefix`、`prefix_mismatch`、`trimmed`、
  `nothing_to_trim` 和 `trim_failed`；同时记录裁剪前后条目数、字节数、覆盖条目数、
  缓存条目数、检查点 ID 哈希和 iteration 覆盖值。检查点缓存仅在内存中，重启会清空。
- 压缩确认：`compaction_seen=true` 表示收到压缩项；`compaction_confirmation=confirmed`
  表示终态通过验证并登记了检查点；`not_confirmed` 表示收到但未登记；
  `payload_input_changed` 表示最终 payload 规则修改了输入，因此跳过确认。

桥接校验失败时，会立即写一条 **`BPS response validation failed` warning**，保留具体的
`validation_reason` 和阶段。例如 `tool_translation` 是工具还原失败，
`terminal_tool_completeness` 是终态遗漏了此前的工具项，`structured_output` 是结构化输出
校验失败。还会记录原生工具名称和类型、可解析的客户端目标工具名、参数类型和长度、
传输封装形态、待确认工具数，以及最多 8 个遗漏工具的名称和身份哈希。
warning 中的压缩状态可能仍为 `pending_validation`，应结合结束时的 timing 日志判断。

按 CPA 请求 ID 关联 warning、timing 和访问日志即可排查；客户端收到的错误文案不变。
例如查阅日志文件（路径按实际配置调整）：

```sh
rg 'BPS response validation failed|BPS request timing' logs/main.log
rg '\[请求短ID\]' logs/main.log
```

日志不包含提示词、代码正文、工具参数值、凭据、加密压缩内容或外部解析器原始错误。
检查点和调用身份使用 SHA-256 哈希；过长或格式异常的工具名也只记录哈希。
`native_arguments_bytes=-1` 表示原生参数不是字符串，未计算序列化大小。
工具目录来自最终 payload 规则应用前的桥接目录；`payload_input_unchanged=false`
表示最终规则改动了输入。日志本身不修改请求、压缩阈值、工具校验、重试或超时策略。

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
