# CodeArts2API

> 华为云 CodeArts Agent（盘古助手/码道）的 OpenAI 兼容代理。**无需运行 CodeArts Agent
> 客户端**，纯 Go 直连华为云 API，多账号轮转 + token 自动续期 + WebUI。

## 参考项目

本项目是 [Sliverkiss](https://github.com/Sliverkiss) 同系列开源项目的延伸实现，架构与运维形态参考了以下仓库：

- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy CN OpenAI 兼容反代（账号池 / 轮转 / 签到架构）
- [traework2api](https://github.com/Sliverkiss/traework2api) — TRAE Work OpenAI 兼容反代（零依赖 Go 骨架）
- [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) — QoderWork CN OpenAI 兼容反代（OAuth 授权流程）

感谢原作者的开源与优秀设计。

## 快速开始（Ubuntu / Linux）

```bash
make linux            # bin/ 下 4 个 Linux 静态二进制
make test
```

### 登录（华为云账号）

```bash
# 本机有浏览器
./login.sh

# 服务器（无浏览器）：打印链接，任意机器浏览器打开，ticket 轮询下发
./login.sh -print-only

# 凭证落盘 auths/codearts-{user_id}.json
```

### 启动

```bash
cp config.example.json config.json
export CA2A_API_KEY=你的随机密钥
./bin/codearts2api -config config.json
```

### 验证 + WebUI

```bash
curl http://127.0.0.1:7866/healthz
curl http://127.0.0.1:7866/v1/models -H "Authorization: Bearer $CA2A_API_KEY"
curl -X POST http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $CA2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"snap-chat","messages":[{"role":"user","content":"你好"}]}'
```

浏览器打开 **http://127.0.0.1:7866/** 即 WebUI：账号/token 状态、对话测试（流式/非流式）。多轮上下文按账号自动续接（chat_id 分组）；也可用请求头
`X-Codearts-Chat-Id: <chatId>` 或 body 里 `conversation_id` 显式指定会话。

流式请求设置 `"stream_options":{"include_usage":true}` 后，会在 `[DONE]` 前返回一次
`choices: []` 的 usage chunk；其余 chunk 的 `usage` 为 `null`。未启用时不返回 usage。
流式和非流式均优先使用上游 usage；上游未提供时按文本长度估算，估算值不是精确 token 计数。

工具调用支持 `tool_choice` 的 `auto` / `none` / `required` 及指定函数对象。
指定函数时，网关只向上游发送该函数的定义，并转换为上游支持的字符串 `required`；
指定函数不存在或重复定义时返回 400。上游 `InferHub.001001005` 参数错误也返回 400，
不轮换、不冷却账号；若流式响应已开始，则用 SSE 错误事件结束，同样不处罚账号。

### 排队、限流与 sub2api 映射

单账号默认并发为 **1**。早期示例配置写了 `max_concurrent: 5`，已有部署需手动改成 1；
更新源码不会覆盖挂载的 `config.json`。

```jsonc
"max_concurrent": 1,
"queue_max_attempts": 3,   // 包含首次请求，默认最多等待两次，每次 10 秒
"queue_retry_seconds": 10
```

等待上游重试时保留本请求的并发槽位，客户端取消即停止。遇到限流时有其他空闲且
目录匹配（或目录未知）的账号会优先切换；最后一次失败后不再等待，返回 HTTP 429
及 `Retry-After`。上游要求更长的等待时直接回传退避，不提前重试。账号只作短暂限流
退避，不累计故障计数或进入 10 分钟错误冷却。本地槽位等待最多 30 秒，且响应客户端取消。

sub2api 的映射目标须填写实际模型 ID，例如 `deepseek-v4.1-flash-1017`；版本后缀会
完整保留，服务不会自动替换成 `deepseek-v4.1-flash`。两个 ID 的可用性与参数要求
应分别验证。映射后失败时，先用目标 ID 直接请求以区分映射路径与目标模型问题。

参数 400 会记录一行 `upstream params rejected ... request_shape={...}`，仅含模型、
渠道、生成参数和消息/工具数量，不含正文、工具定义或凭据。可用它核对 `requested_model`、
`resolved_model`、`reasoning_effort`、`max_tokens` 与已发现的 `model_output_limit` 及其来源。
排队日志也会保留上游错误摘要，用于区分 TPM 限流和并发会话限制。

### 输出预算与参数 400

输出预算遵循**当前账号所选渠道的 CodeArts 模型目录**。与桌面客户端 0.2.6 一致，
优先使用正数 `output_context_window`，缺失时回退到 `max_tokens`；已知预算也会通过
`/v1/models` 的 `max_output_tokens` 返回。带输出预算的聊天请求遇到目录缺失或过期时，
只为所选账号补查一次，最多等待 10 秒；并发聊天合并发现，失败后退避 5 分钟。

`max_tokens` / `max_completion_tokens` 超过目录声明的预算时会自动收窄，并记录
`max_tokens capped ... channel=... limit_source=... requested_max_tokens=... effective_max_tokens=...`。
换账号时重新按原始预算计算；未传预算或渠道未提供预算时保持原样，不推测固定上限。

2026-10-30 本地 Agent IDE 26.10.100 日志中，`deepseek-v4.1-flash` 与
`deepseek-v4.1-flash-1017` 的配置均为 1M 上下文、65536 输出预算。IDE 直接读取
`max_tokens`，桌面端则优先读取 `output_context_window`，因此两者可能显示不同预算。
这份 IDE 日志**不能证明服务端固定限制为 64K，也没有确认它就是参数 400 的根因**；
同样，DeepSeek 直连 API 的 384K 规格不能证明当前 CodeArts 账号或渠道支持该数值。

参数 400 的诊断同时记录原始 `model_max_tokens`、`model_output_context_window`、
实际采用的 `model_output_limit` / `model_output_limit_source` 及 `model_context_window`。
若仍被拒绝，需结合这些字段继续核对工具定义和消息格式；工具或消息数量本身不足以定因。

### 模型列表与客户端权益渠道

`/v1/models` 返回上游**精确推理 ID**（区分大小写，如 `GLM-5.2`、`glm-5.3`），
同时为含大写的 ID 补一条小写别名（`glm-5.2`），两者都能用于聊天。
展示名可能与推理 ID 不同：官方 Agent 26.10.100 的 `GLM-5.3` 实际调用 `glm-5.3`，
`GLM-5.2-ArkTS-SPARK` 实际调用 `glm-5.2-sft-harmony`。发现时优先读取
`model_parameters.model_id`，并按实际账号解析展示名及大小写，避免其他账号覆盖调用 ID。

福利和教育渠道模型额外带 `benefit: true` 及 `maas_type: benefit / education` 标记。
列表是各账号模型的并集；聊天与探测均按实际使用账号的目录解析渠道，在签名前追加
上游要求的 `maas_type` 请求头。

同 ID 的普通、福利和教育记录独立保存，预算不跨渠道拼接。只提供模型 ID 时，
与官方 IDE 的普通目录优先规则一致：先选普通记录；没有普通记录时，沿用
`benefit → education` 的兼容顺序，路由与预算一起选择。`/v1/models` 每个 ID 展示
默认记录，探测每个默认记录一次；可用性缓存按账号、模型和渠道隔离。
`cmd/models -json -claim=false` 可查看所有渠道的原始参数。

CodeArts 网页端使用 `/chat/.../cloudagent/sessions/.../messages` 的会话协议；
Agent IDE 与桌面客户端使用 `/api/v2/chat/completions`。本项目沿用客户端协议。
网页会话成功不能直接证明兼容 API 的某组生成参数有效；当前尚未确认网页端的输出预算。

福利模型列表随免费套餐轮换，用下面这条命令核对当前账号实际可用的模型：

```bash
go run ./cmd/models                 # 账号模型（内置 + 已启用的福利/教育渠道）
go run ./cmd/models -json           # 机器可读
go run ./cmd/models -claim          # 仅对已启用渠道领取后查询（幂等，属写操作）
go run ./cmd/models -claim=false    # 不自动领取，仍查询已启用渠道
```

对齐 CodeArts 桌面客户端 0.2.6：先查询 `/v1/benefit-gateway-config` 与
`/v1/education-gateway-config`，只有 `enabled=true` 才查询相应模型并自动领取。
**关闭渠道不会调用旧领取接口，也不会从静态种子补回过期福利模型**；这不代表普通
内置模型停用。开关或模型接口临时失败时保留上次已确认的渠道模型；业务错误不会被当作关闭。

尚未领取或已经过期的权益可能返回 `InferHub.4004.200 benefit not found`。服务默认
自动领取已启用渠道（`benefit_auto_claim: true`）；不想让服务写账号可显式关闭：

```jsonc
"benefit_auto_claim": false   // 或 CA2A_BENEFIT_AUTO_CLAIM=0
```

关闭自动领取不影响已领取的有效权益。模型是否能调用仍取决于当前账号权限、额度及探测结果。

### 模型可用性探测

`/v1/models` 会过滤已确认不可用的模型；尚未探测的条目仍会保留。服务周期性用一条
最小请求探测，真实请求的结果也会写入缓存，展示名与推理 ID 共用同一账号的结论。
`InferHub.002002009.404 / not registered` 也可能由错误 ID、大小写或渠道导致，
**不能仅凭这条错误判断账号需要注册模型**。官方客户端能回复而代理失败时，应先核对
模型映射与渠道。模型错误不会给健康账号记错误冷却；探测结果缓存 30 分钟。

### API Key 必填

服务没有 API Key 会**拒绝启动**（旧版本会回退到公开的 `dummy-key-for-codearts`，
等于把接口暴露给任何人）。三选一：

```bash
# 1) env（推荐，配合 .env / systemd EnvironmentFile）
export CA2A_API_KEY=$(openssl rand -hex 24)
# 2) config.json 的 "api_key" 字段
# 3) docker compose 的 .env
```

### 账号续期

`refresh_token` 与登录时的 `client_id`、DPoP 私钥绑定，因此凭证文件会保存
`client_id` 与 `dpop_private_key`；刷新时原样复用并写回轮转后的新 refresh_token。
若手工换过 `login_client_id`，老账号仍按自己记录的 client_id 刷新。

### 每日签到与新手积分

服务默认在启动和每次 watchdog 轮询时检查活动，自动领取每日签到积分及
上游返回的可领取新用户积分。新增账号会在下一次轮询处理；也可在 WebUI
点击「签到」/「全员签到」，或鉴权调用 `POST /admin/api/checkin`（可传 `{"uid":"账号 ID"}`）。
领取后必须确认；已领取未确认时只补确认，已确认不重复领取。资格、金额、
每日刷新和活动起止时间均以上游返回为准，失败会在下一次轮询重试，
不会因活动接口故障冷却或禁用推理账号。

`credits_auto_claim: false` 或 `CA2A_CREDITS_AUTO_CLAIM=0` 可关闭自动领取，
`watch.enabled: false` 会关闭整个轮询。手动签到仍可使用。
这项活动与 `benefit_auto_claim` 的模型 Token 福利独立；面板显示最近领取结果，
不把领取量或健康账号数当作积分余额。`credit.sh` 查登录态，`apply.sh` 仅续期。

当前[官方每日签到活动](https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0004.html)
为已升级积分计费的用户每日发放 1000 积分，30 天有效，活动截止于
2026-12-31 00:00（UTC+8）；[新用户注册奖励](https://support.huaweicloud.com/price-codeartsagent/codeartsagent_billing_0029.html)
为 4000 积分。无资格或已自动发放的新手奖励不会再次领取。

## 部署（systemd / Docker）

```bash
sudo mkdir -p /opt/codearts2api && sudo cp -r bin config.example.json auths /opt/codearts2api/
sudo cp deploy/codearts2api.service /etc/systemd/system/
# 编辑 /opt/codearts2api/.env 写 CA2A_API_KEY，改好 config.json
sudo systemctl daemon-reload && sudo systemctl enable --now codearts2api

# 或 Docker
export CA2A_API_KEY=你的随机密钥
mkdir -p auths data
docker compose up -d --build
```

## 环境变量配置

除了 `config.json`，还支持以下环境变量覆盖：

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `CA2A_API_KEY` | API 访问密钥 | - |
| `CA2A_LISTEN` | 监听地址 | `:7866` |
| `CA2A_AUTH_DIR` | 凭证目录 | `./auths` |
| `CA2A_STATE_FILE` | 状态文件 | `./data/state.json` |
| `CA2A_DEFAULT_MODEL` | 默认模型 | `glm-5.2` |
| `CA2A_OAUTH_CALLBACK_HOST` | OAuth 回调主机 | - |
| `CA2A_WATCH_ENABLED` | 调度器开关 | `true` |
| `CA2A_WATCH_POLL_MINUTES` | 轮询间隔（分钟） | `30` |
| `CA2A_WATCH_REFRESH_SKEW` | 提前刷新时间（分钟） | `30` |
| `CA2A_WATCH_KEEPALIVE_INTERVAL` | 保活间隔（分钟） | `15` |
| `CA2A_MAX_CONCURRENT` | 单账号最大并发 | `1` |
| `CA2A_QUEUE_MAX_ATTEMPTS` | 包含首次请求的上游总尝试次数 | `3` |
| `CA2A_QUEUE_RETRY_SECONDS` | 两次上游尝试之间的等待秒数 | `10` |
| `CA2A_KEEPALIVE_WINDOW` | 保活窗口 | `10m` |
| `CA2A_BENEFIT_AUTO_CLAIM` | 发现模型时自动领取限时福利（幂等；关闭则福利模型不可用） | `true` |
| `CA2A_CREDITS_AUTO_CLAIM` | watchdog 自动领取每日签到及新用户积分 | `true` |
| `CA2A_LOGIN_CLIENT_ID` | WebUI 登录使用的 OAuth client_id | 已有账号的取值，否则 `codearts-agent` |

## 目录结构

```
cmd/server/        HTTP 服务（config + main）
cmd/login/         华为云 OAuth2 PKCE 登录
cmd/credit/        账号登录态日报（新增并发信息）
cmd/apply/         批量 token 续期（使用 pool 包）
cmd/models/        查看账号可用模型（内置 + 限时福利，可选 -claim 领取）
internal/auth/     auth 文件读写
internal/upstream/ 云端客户端（登录/聊天/SSE/模型发现）+ 逆向常量
internal/pool/     账号池（token 校验/自动刷新/冷却/并发控制）
internal/scheduler/ token 续期看门狗（新增保活机制）
internal/server/   OpenAI 兼容路由
internal/webui/    内嵌 WebUI 控制台
deploy/            systemd unit 样例
docs/              逆向过程与接口清单
```

## 免责声明

仅供学习和研究使用。使用者需遵守华为云服务条款，自行承担使用风险。

## License

MIT

