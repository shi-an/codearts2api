# CodeArts Agent 逆向记录（2026-08-03，版本 26.7.0）

> 素材：`D:\CodeArts Agent\resources\app`（VS Code 系，未打包目录），
> 扩展 `vscode-codebot`（agent + snap API 客户端）、`huaweicloud.authentication`
> （登录）、`huaweicloud.codearts-agent-support`，以及 `product.json` 域名表。

## 1. 架构

CodeArts Agent 是 VS Code 系 Electron 应用（vscode 1.109.5 定制）。AI 对话走
**华为云 snap-access 盘古引擎**：

- 聊天：`POST https://snap-access.cn-north-4.myhuaweicloud.com/v1/chat/chat`（SSE）
- 登录：华为云 CodeArts OAuth2（PKCE），`snap-manager/v1/oauth2/tokens` 换 STS 临时凭证
- 鉴权：聊天用 `x-auth-token: <security_token>`；部分账号 API 用临时 AK/SK 签名

客户端里还带一个 Cline 系 agent 内核（`agentkernelServer-*.exe`，Bun 打包的本地
HTTP 服务），负责本地 agent 循环（会话/工具/文件），云端对话仍是上面的 chat 接口。

## 2. 关键端点（商业版 cn-north-4）

| 用途 | 方法/路径 | 鉴权 |
| --- | --- | --- |
| 聊天 | POST /v1/chat/chat | x-auth-token: security_token |
| 开始聊天 | POST /v1/chat/start-chat | 同上 |
| Agent 列表 | GET /v1/chat/agents | 同上 |
| 反馈事件 | POST /v1/chat/management/event | 同上 |
| 记录请求 | POST /v1/chat/record-request | 同上 |
| 换 token | POST /v1/oauth2/tokens（authorization_code / refresh_token） | client_id + code/refresh_token |
| ticket 轮询 | GET /v1/login/ticket?ticket_id=&secret= | plugin-name/version 头 |
| 当前用户 | GET /snap-manager/v1/current/user | AK/SK 签名 + X-Security-Token |
| 账号信息 | GET /v5/caller-identity（sts.cn-north-4） | AK/SK 签名 + X-Security-Token |

身份接口实测修正（2026-09-15，两个都踩过坑）：
- `current/user` 必须带 `snap-manager` 前缀：漏掉返回 `APIG.0101 The API does not exist`。
- `caller-identity` 只在 `sts.cn-north-4` 域可用：`iam.myhuaweicloud.com/v5/caller-identity`
  返回 `APIGW.0101`。
- 返回字段：caller-identity → `{account_id, principal_id, principal_urn}`；current/user →
  `{user_id, user_name, domain_id, domain_name}`。

域名表（product.json commercialVersionDomain.newFramework.productDomain）：
`chatDomain = codeGenDomain = snapEngineDomain = snap-access.cn-north-4.myhuaweicloud.com`；
`toolkitDomain = iam.myhuaweicloud.com`；`iamStsOpenDomain = sts.cn-north-4.myhuaweicloud.com`。

## 3. 登录流程（huaweicloud.authentication）

1. 生成 `ticket_id`（32 hex）、`secret`（32 hex）、PKCE `code_verifier/code_challenge`。
2. 本地监听 `127.0.0.1:{port}/oauth/callback`。
3. 构造 `https://codearts.huaweicloud.com/authorize?client_id=...&port=...&code_challenge=...&code_challenge_method=S256&ticket_id=...&plugin-name=huaweicloud.authentication&plugin-version=...`。
4. 双通道拿结果：
   - 回调带 `code` → `POST snap-manager/v1/oauth2/tokens`
     `{client_id, code, code_verifier, grant_type:"authorization_code", redirect_uri:"http://127.0.0.1:{port}/oauth/callback"}`
   - 轮询 `GET snap-manager/v1/login/ticket?ticket_id=&secret=`（插件头）
5. 响应含 `{user_id, user_name, domain_id, refresh_token, credentials:{access_key_id, secret_access_key, security_token, expiration}}`。
6. 刷新：`POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`
   `{client_id, code_verifier, grant_type:"refresh_token", refresh_token}`（同样带 DPoP）。

`CLIENT_ID` = 应用 `uri_scheme`，实测为 **`codearts-agent`**（官方 CodeArts Agent 插件）。

**refresh_token 与 client_id、DPoP 公钥三者绑定**，实测两种失败：
- client_id 不匹配 → `400 STS5.1806 invalid refresh token: 'invalid client id: codearts-agent'`
- DPoP 私钥换新 → `400 STS5.1806 invalid refresh token: 'InvalidDPoPHeader'`
- refresh_token 是一次性的，用旧值再刷 → `invalid refresh token: 'the refresh token has been used'`

因此登录时的 client_id 与 DPoP 私钥必须随 refresh_token 一起落盘（`auths/*.json` 的
`client_id` / `dpop_private_key` 字段），刷新时原样复用，并把响应里轮转后的新
refresh_token 写回。

## 4. 聊天请求

Header：
```
Content-Type: application/json
Accept: text/event-stream
X-Sdk-Date: <YYYYMMDDTHHMMSSZ>
X-Security-Token: <STS security_token>（临时凭证时）
X-Sdk-Content-Sha256: <payload sha256>
Authorization: SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=..., Signature=...
x-snap-traceid: <随机>
```

**鉴权不是 x-auth-token**：实测 x-auth-token 传 STS security_token 会被 APIG 拒
（APIG.0301 decrypt token fail）。桌面端走 `GlobalCredentialClient`，即华为云
AK/SK `SDK-HMAC-SHA256` 签名（signed headers = 请求全部头，CanonicalURI 带尾斜杠，
payload hash 取 X-Sdk-Content-Sha256）。已按该算法实现并实测通过。

Body：
```json
{"chat_id":"<32位hex>","client":"IDE","task":"chat",
 "messages":[{"type":"text","text":"..."}],
 "task_parameters":{"ide":"CodeArts Agent"},
 "batch_task_parameters":[],"attempt":1,"not_allow_external_model":true,
 "user_id":"<用户名>"}
```

实测要点：
- `chat_id` 必须是 **32 位十六进制**（UUID 去连字符），否则报「请求参数错误：chat_id」
- `messages` 是 **内容块数组（无 role）** `[{type:"text",text}]`，
  不是 OpenAI 的 {role,content}，否则报「请求参数错误：messages」
- 携带 `user_id`（用户名）更稳妥

## 5. SSE 格式

实测为**逐行 `data:` JSON（无空行分隔，部分行也无 event 前缀）**：
- 起始：`{"id":...,"model":"glm-4.7","type":"answer","chat_id":...,"response_message_id":...}`
- 全文快照：`{"text":"<当前完整文本>","output":[],"prompt_tokens":...,"completion_tokens":...}`
  （text 是**累计全文**，非增量，解析时用替换语义）
- 增量：`{"delta":{"content":"...","reasoning_content":"..."}}`（is_delta_response 场景）
- 结束：`{"text":"[DONE]","error_code":"0",...}`；错误形如
  `{"text":"[DONE]","error_code":"ChatAgent.00001001","error_msg":"..."}`
- `output` 数组：终态 `[{type:"output_text",text}]`

## 6. 活动积分与每日签到（2026-10-10 更新）

早期的「无每日签到」结论已不适用。积分活动和第 7 节的模型 Token 福利是独立账本。
活动接口位于 `https://snap-access.cn-north-4.myhuaweicloud.com` 根路径，
**不带 `/snap-manager`**；使用 AK/SK 签名、`X-Security-Token`、`Agent-Type: PromptCenter`。

- `GET /v1/ops/delivery?channel=IDE`：`{code:0,data:{items:[...]}}`。按活动 ID
  选择每日签到（1）和新用户（4），不自动参与学生认证、邀请等其他活动。
- `POST /v1/ops/claim`：`{campaignId,channel:"IDE",idempotentKey:"claim_<ID>_<毫秒时间戳>"}`。
- `POST /v1/ops/confirm`：`{campaignId}`。
- `ELIGIBLE` 且 `claimable=true` 才领取；`CLAIMED` 只补确认；
  `CONFIRMED` / `CONSUMED` 已完成。HTTP 200 仍必须检查业务 `code=0`。
- 活动起止时间取 `extra.startTime/endTime`；金额取 `pendingTotalAmount/benefitAmount`。
  delivery 还含活动跳转票据，日志/面板只使用脱敏字段，不保存或显示这些票据。
- 调度器和手动签到按账号串行执行；失败不影响推理账号健康，下一轮继续查询。

官方说明：[每日签到得积分](https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0004.html)、
[个人订阅及新用户积分](https://support.huaweicloud.com/price-codeartsagent/codeartsagent_billing_0029.html)。
协议参考并以当前账号的 delivery/claim/confirm 验证：
[Coding2API CodeArts 客户端](https://github.com/RobbsLuo/Coding2API/blob/main/src/provider/codearts/client.py)。
`credit.sh` 保留为登录态查询；`apply.sh` 仍仅进行 token 续期。

## 7. 限时福利模型（免费套餐，2026-09-06 逆向自 CodeArtsSpace 0.1.18）

> 该节由 PR #2（@threegod3）的逆向结论整理而来。

官方客户端可用模型 = 内置 + 限时福利两路合并（`ModelService.listBuiltinModels` + `getFreeBenefitModels`）：

- 福利发现：`GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config`
  （AK/SK 签名 + `X-Security-Token`，无 Agent-Type），返回
  `{error_code:"0000", result:{base_url, models:[{model_id, model_name, context_window, max_tokens}]}}`。
  实测模型 ID 全小写：`deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash`。
- 领取/余额（幂等，官方客户端打开模型菜单即调用）：
  `POST /api/v1/benefit/claim {}`、`GET /api/v1/user/tokens/balance`（同 host 同签名）。
- 网关总开关：`GET {snap}/v1/benefit-gateway-config`（`Agent-Type: PromptCenter`）→ `{enabled}`。
- 聊天路由：**同一** `POST {snap}/api/v2/chat/completions`，福利模型需追加请求头
  `maas_type: benefit`（renderer 检测 `isFreeBenefit` 添加，经 kernel `KERNEL_LLM_FORWARD_HEADERS`
  转发）。无此头报 `InferHub.002002009.404 model is not registered`；有头正常流式。
- agent 选择：`useragents?offset=0&limit=100&is_primary_agent=true` 后取
  `agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide`（退化 primary/首个）。
- 内置补充接口：`GET {snap}/v1/model/builtin`（`Agent-Type: PromptCenter`）→ `{builtinModels:[...]}`。
- 模型 ID 区分大小写；福利网关返回小写，`CanonicalModel` 做大小写不敏感归一。

### 本仓库的实现取舍

- **福利目录按账号存放**（`internal/upstream/models.go`）：福利是按账号授予的，
  池里 A 账号有、B 账号没有时，判定依据必须是**发起请求的那个账号**，不能全局共享；
  目录**整体替换**而非增补，套餐轮换后不留旧标记。
- **发现结果优先于冷启动种子**：目录里标了福利 → 带 `maas_type`；目录里有但没标
  （说明它以内置/agent-center 身份注册）→ 不带，福利模型转正后能自动摘掉该头；
  目录里查不到（未发现、福利来源失败或未领取）→ 回退种子带头，避免未注册模型 404。
- **账号目录与请求映射**：`/v1/models` 展示各账号目录的并集，不代表每个账号都拥有
  列出的全部模型。聊天发送的精确 ID 与渠道必须由实际使用的账号解析；
  可用性结论同样按账号保存，模型级错误不应触发账号故障冷却。
- **`maas_type` 由服务端注入并计入签名**：`SendChatV2` 在 `signRequest` 之前设置该头，
  SignedHeaders 覆盖它（与官方客户端行为一致）。
- **领取默认开启**：实测未领取时福利模型一律返回 `InferHub.4004.200 benefit not found`，
  领取后同一模型立即可用（`glm-5.3-flash`/`deepseek-v4-flash-0731`/`deepseek-v4-pro-0813`
  均验证通过）。领取是幂等操作，官方客户端打开模型菜单即调用，因此默认执行；
  可用 `benefit_auto_claim=false` 关掉。
- **发现结果不保证推理可用**：权限、额度、权益有效期及请求映射都会影响结果。
  早期用 `GLM-5.2-ArkTS-SPARK`、`OpenPangu-2.0-Pro/Flash` 展示名调用得到的
  `InferHub.002002009.404`，不能作为账号缺少模型权限的证据：26.10.100 客户端中，
  它们对应不同的推理 ID（见下表）。福利未领取仍可能返回 `4004.200`。
  探测必须使用正确的推理 ID 与渠道；未探测的模型也会保留在列表中。
- **权益来源失败时保留上一轮条目**：临时失败时按 ID + 渠道保留上一次确认的记录；
  普通目录也出现相同 ID 时，默认选择普通记录，权益记录仍独立保存。成功发现会替换
  相应渠道的旧记录，明确关闭会移除该渠道记忆。
- 冷启动种子（`seedBenefitModels`）：首次发现前也要带福利头，少带一次就是一次 404。

### 2026-10-30：对照桌面客户端 0.2.6

本次依据已安装官方客户端 `resources/app.asar` 中的 `out/main/index.js`
（`ModelService` / `BenefitService`）与 renderer 的 `channelOf`、
`AgentKernelAdapter` 核实。上述 2026-09 的套餐和实测结果是历史记录，不能据此承诺当前账号仍有相同权益。

- **先检查开关**：策略中心 `/v1/benefit-gateway-config`、
  `/v1/education-gateway-config` 均须签名并带 `Agent-Type: PromptCenter`。
  支持顶层 `enabled` 或 `result.enabled`；只有明确启用的渠道才进入领取与模型发现。
- **按渠道查询**：默认福利继续使用不带 channel 的 `/api/v1/gateway/config`；
  教育使用 `?channel=education`。领取教育权益时也传该参数，领取请求需签名
  `Agent-Type: PromptCenter`；模型发现请求沿用客户端的无 Agent-Type 形式。
- **渠道元数据**：发现结果保留 `maas_type=benefit/education`；聊天仍使用
  客户端 `/api/v2/chat/completions`，按当前账号目录在签名前加入渠道头，普通模型不带。
  `cmd/models -json` 保留同 ID 的各渠道记录，参数仅在 ID 与渠道都相同时合并。
  只提供 ID 时普通记录优先；没有普通记录时沿用 benefit、education 的兼容顺序。
  `/v1/models` 展示默认记录，聊天、预算与探测使用同一默认渠道。
- **关闭与故障分开处理**：明确关闭时丢弃该渠道历史条目，并禁止静态福利种子补位；
  超时、HTTP/业务错误、无效响应或退避属于未知状态，跳过本次领取/发现并保留已确认条目。
  单个渠道失败不影响其他来源成功返回的模型；返回给调用方的列表与保存的目录一致。
- **移除结果不再复活**：成功发现时替换旧渠道记忆；后续临时失败不能重新带回已经删除的模型。
- **验证范围**：回归测试覆盖开关、渠道参数、请求头签名、账号隔离和部分发现失败。
  本次未用真实账号验证新模型权限，启用渠道或模型发现成功都不等于推理可用。

### 2026-10-30：对照 CodeArts Agent 26.10.100 的模型 ID

依据当前安装的官方客户端 `resources/app/extensions/vscode-codebot/out/extension.js`
及其模型目录日志，确认客户端优先使用 `model_parameters.model_id`，其次使用
`model_alias`；`model_name` 用于显示。模型元数据如下：

| 展示名 | 推理 ID |
| --- | --- |
| `GLM-5.2` | `GLM-5.2` |
| `GLM-5.3` | `glm-5.3` |
| `GLM-5.2-ArkTS-SPARK` | `glm-5.2-sft-harmony` |
| `OpenPangu-2.0-Pro` | `openpangu-2.0-pro` |
| `OpenPangu-2.0-Flash` | `openpangu-2.0-flash` |

- agent-center 的解析顺序改为推理参数 ID、别名、顶层 ID、展示名；内置与权益来源
  同样分开保存 ID 与展示名。合并参数时保留优先来源的 ID，不再偏好大写。
- 发现结果覆盖冷启动种子的大小写，展示名作为兼容别名；同一账号已有真实 ID 时，
  其他模型的同名展示别名不能覆盖它。聊天、探测和命令行工具均按本次账号解析。
- 推理仍使用客户端的 `POST /api/v2/chat/completions`，普通模型不因 ID 错误而额外
  添加福利头。`not registered` 的日志说明改为核对模型 ID 或路由，不再要求用户注册。
- 回归测试复现了 `glm-5.3` 被转换成 `GLM-5.3` 的错误，并验证发现、完整聊天请求、
  探测、渠道签名、跨账号大小写隔离和别名共用可用性缓存。
  本次使用本地模拟服务验证修复，未使用真实账号进行云端推理。

### 2026-10-30：CodeArts Agent 26.10.100 的 DeepSeek 输出预算

从客户端日志的 `AgentCore model-config update` 完整 JSON 按模型键提取，两个条目分别为：

| 推理 ID | `limit.context` | `limit.input` | `limit.output` |
| --- | --- | --- | --- |
| `deepseek-v4.1-flash` | 1048576 | 1044480 | 65536 |
| `deepseek-v4.1-flash-1017` | 1048576 | 1044480 | 65536 |

扩展的 `updateModels` 将当前目录的 `model_parameters.max_tokens` 直接写入
`limit.output`，`context_window` 写入 `limit.context`。这里是该客户端当时收到的配置，
不代表模型厂商直连 API 的输出规格，也不保证所有账号、渠道或后续版本一致。
原始 `models info` 日志可能被截断，不能跨模型搜索后把相邻 GLM 的字段误当 DeepSeek 参数。

65536 是本地 IDE 当时使用的预算，尚未通过真实推理确认服务端拒绝阈值；这份日志也未能
确认用户 Linux 部署账号的实际配置，不能据此把所有参数 400 归因于超过 64K。

服务按当前账号所选渠道的目录预算调整请求；跳过 `/v1/models` 的带预算请求会补查目录，受请求取消和
10 秒总超时约束。取消或总超时不写入半份目录；已有参数仍可用于预算检查，未知上限保持
原请求。回归用本地模拟服务覆盖 214057 预算、86 工具和 155 消息的参数调整、账号轮转、
并发发现与失败退避；未使用真实账号确认该调整是否消除所有云端 400。

### 2026-10-30：桌面端输出字段与同名渠道记录

继续核对官方 CodeArts 0.2.6 的 `out/main/index.js` 与
`out/renderer/assets/index-MEKoVEbG.js`，发现两处需要兼容的差异：

| 入口 | 已核实的请求协议 | 输出预算读取方式 |
| --- | --- | --- |
| CodeArts Agent IDE 26.10.100 | `/api/v2/chat/completions` | `model_parameters.max_tokens` → `limit.output` |
| CodeArts 桌面端 0.2.6 | `/api/v2/chat/completions` | `output_context_window` 优先，回退到 `max_tokens` |
| CodeArts 网页端（先前抓取） | `/chat/.../cloudagent/sessions/.../messages` | 尚未核实，不能由客户端目录推断 |

- 桌面端 `flattenBuiltinModels` 的 context 顺序为 `context_window`、
  `input_context_window`；output 顺序为 `output_context_window`、`max_tokens`。
  代理在 agent-center、内置和权益来源中保留两个输出字段，正数预算采用相同优先级。
  未使用客户端本地默认常量推测未知账号的预算。
- 桌面端把普通和权益模型数组拼接，`findModel(modelId, maasType)` 可按二者共同选择。
  IDE 的 `isCurrentModelBenefitType` 在普通目录匹配同名 ID 时返回 false。
  旧代理按 ID 合并并对权益标记取或，会拼出普通参数加权益路由的记录，现改为
  按 ID + 渠道合并。默认普通优先，只有权益记录时沿用 benefit、education 顺序；
  该顺序是兼容接口的默认选择，不能据此推断哪个渠道额度更大或排队更短。
- 内置目录自带的 `is_free_benefit` / `maas_type` 也参与渠道解析，不能仅凭数据来源
  把条目标成普通模型。渠道缓存独立保留；关闭或成功返回空目录后不会被后续失败复活。
- 输出预算、`/v1/models` 与探测使用默认记录；可用性缓存按账号 + ID + 渠道隔离，
  渠道变化后不沿用另一渠道的不可用结论。相同 ID 的多条记录只触发一次默认渠道探测。
- 400 诊断保留 `model_max_tokens`、`model_output_context_window`，并增加
  `model_output_limit` / `model_output_limit_source`；收窄日志也记录渠道和来源字段。

本地模拟回归覆盖四路解析、同渠道字段补齐、字段优先级、跨渠道隔离、部分失败与关闭、
高于 64K 的预算透传、实际请求渠道签名和诊断脱敏。393216 等测试数值为合成样本，
尚未确认用户账号的 DeepSeek 目录是否提供更大的 `output_context_window`；
代理侧未直接进行真实云端推理。

随后用户部署复测确认：更新代理后，自动收窄到 64K 的请求已正常返回；成功路径使用了
代理的预算调整。该反馈验证了修正的实际效果，但尚无新的实时目录或完整请求日志，
也未测试大于 64K 时的精确接受阈值，不能据此认定所有账号或渠道都固定限制为 64K。

## 8. 脱敏

本仓库不包含任何真实 token。`auths/`、`data/`、`config.json`、`.env` 均 gitignore。
