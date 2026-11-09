# meoo2api

Meoo 模型网关的 OpenAI 兼容代理，附精简账号管理控制台。Go 单二进制，无 Node.js、Chromium 或浏览器自动化运行依赖。

```text
OpenAI 客户端 → http://host:8791/v1
                        ↓ 可用账号的项目 AK
https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 添加账号

打开控制台 → **账号管理 → 添加账号 → 浏览器授权登录**。

1. 服务端创建 Meoo CLI 设备授权会话。
2. 在你自己的浏览器打开 `https://meoo.com/cli/login?session_id=…`，登录 Meoo 后确认授权。正确路由为 `/cli/login`，不是 `/cli-login`。
3. 服务端轮询授权结果、验证真实身份，保存个人 AK，自动建立项目、开通云服务并取得项目 AK 入池。
4. 同一 Meoo 身份重复接入会更新凭证并复用账号和项目；开通中断后「重试接入」从已记录的项目继续。若 Meoo 返回云实例额度已满，会在有界项目列表中查找本人已有的已开通项目，仅读取其环境凭证用于复用，不删除或改动远端项目；未找到则显示具体额度错误。

核心接入流程不需要命令行、公共 OAuth 回调地址或手工拷 Cookie。若授权页因未登录跳回 Meoo 首页，先登录，再重新打开弹窗中的授权链接。

个人 AK、项目 AK 与网页 Cookie 是三种独立凭证：

| 凭证 | 用途 |
| --- | --- |
| 设备授权签发的 `meoo_ak_…` | 身份、项目与云服务管理、查询积分；不是模型调用密钥 |
| 项目服务 `meoo_project_…` | 调用模型网关；进入账号池 |
| 网页 Cookie | 每日签到与网页会话保活；设备授权不会返回它 |

Cookie 是可选的补充入口。在「账号管理」对应账号操作栏点击 **补充 Cookie / 更新 Cookie**，粘贴该账号的完整 Cookie 后点击「验证并保存」。服务端会核对身份，属于其他账号或已失效的 Cookie 不覆盖现有登录态；该操作只补充登录态，不创建账号、项目或 Key。设备授权账号照常调用模型与刷新积分；未绑定 Cookie 时界面明确显示签到不可用。Cookie 失效不等于项目 AK 失效。

设备授权的上游 `approved` 凭证只能读取一次。本服务串行轮询并缓存结果，避免重复读取或并发请求丢失凭证。取消、登出或更换密码会停止对应的控制台授权任务。

## API

调用凭证为控制台 **API Key** 页签发的 `sk-…`，也可直接使用自带的项目 AK（BYOK）。

```bash
curl http://127.0.0.1:8791/v1/chat/completions \
  -H 'Authorization: Bearer sk-YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen","messages":[{"role":"user","content":"你好"}]}'
```

| 方法 | 路径 | 实现范围 |
| --- | --- | --- |
| POST | `/v1/chat/completions` | 消息、工具调用、流式、推理字段透传；`n` 为 1–4，由本地并发模拟 |
| POST | `/v1/completions` | 文本 prompt/文本数组、非流式 `n`、单 prompt 流式转换 |
| POST | `/v1/responses` | 无状态文本/图片输入、function tools/调用结果、结构化文本格式、流式事件转换 |
| GET | `/v1/models`、`/v1/models/{id}` | 自动同步上游模型目录，转换为 OpenAI 格式，并附可用别名 |
| GET | `/healthz` | 进程状态、当前池容量和告警；不是逐账号实时探活 |
| GET | `/` | 控制台 |

- 流式断开、格式异常、上游错误会被记录，不伪装为成功结束。客户端取消不处罚账号密钥。
- Responses 是 Chat 适配层，不提供保存/查询历史响应、`previous_response_id`、持久化 conversation、后台任务或内置 web search/file search；这些参数返回明确错误。
- Legacy 的 token 数组、`echo=true`、`suffix`、`logprobs` 与流式 prompt 批次不支持，返回明确错误。
- embeddings、图像生成、音频等未实现接口返回 404。上游是否支持特定模型的图片/工具/推理能力，以真实模型响应为准。
- `n>1` 与 prompt 批次会触发多个真实上游请求，相应累计 token 用量。

## 模型自动同步

- 启动后立即请求上游 `GET /models`，之后每 5 分钟自动同步；缓存到期时访问模型目录或控制台状态也会触发合并刷新。
- 兼容 Meoo 原生 `{models: [...], defaultModel: ...}` 和 OpenAI `{data: [...]}` 两种响应，统一对外返回 OpenAI 模型条目。
- 成功结果全量替换：上游新增模型会出现，下架模型会移除，不把过期内置模型混入成功目录。
- 别名只在目标模型仍存在且未隐藏时展示。使用完整模型 ID 的推理请求直接透传，不依赖目录硬编码白名单。
- 同步失败保留最近成功的目录并标记过期；从未同步成功时才回退内置目录。失败后至少退避 30 秒。内存缓存重启后重新同步，不把凭证持久化到缓存。
- BYOK 按凭证摘要隔离缓存，不污染号池目录；最多 64 条缓存。号池同步最多尝试 3 个不同的可用项目 Key，不计入模型请求用量，也不改变 Key 健康状态。
- `/v1/models` 响应头 `X-Meoo-Models-Source`、`X-Meoo-Models-Stale` 表明来源与过期状态；控制台状态 API 的 `modelSync` 包含同步时间、默认模型和错误概况。模型能力仍以上游实际响应为准。

## 长回复与输出上限

默认 **`defaultMaxTokens=0`**，表示客户端未提供输出上限时，网关不额外注入 `max_tokens`，由上游采用自己的默认预算。设置页可保存 0 或一个正整数。

- 客户端显式提供的 `max_tokens` / `max_completion_tokens` / Responses `max_output_tokens` 优先，不会被网关默认值覆盖。
- 旧安装的设置会原样保留；若仍是旧值 **800**，到「设置 → 默认 max_tokens」改成 **0** 并保存。推理模型的思考 token 也会消耗输出预算，800 尤其容易在正文完成前耗尽。
- `finish_reason: "length"` 表示达到输出预算，不代表完整回答；Responses 对应 `status: "incomplete"` 与 `incomplete_details.reason: "max_output_tokens"`。
- 真正的上游断流、未正常结束和超时会返回错误，不会拼接虚假的完成事件。服务不自动续写，以免重复扣费或破坏工具调用。
- 默认请求超时为 180 秒，可在设置中调整，最长 900 秒；客户端自身的超时和输出上限也会影响完整性。

## 编译与启动

源码构建需要 **Go 1.26+**，部署机器只需运行生成的二进制。

```bash
./scripts/build.sh
./bin/meoo2api -bind 127.0.0.1 -port 8791 -data data
```

首次启动从 `MEOO2API_ADMIN_PASSWORD` 读取初始密码；未设置则随机生成并仅在标准输出显示一次。已有状态不会因修改该环境变量而重设密码。

```bash
# 重设密码：先停止正在使用该数据目录的服务
systemctl --user stop meoo2api
./meoo2api -data data --reset-password
systemctl --user start meoo2api
```

同一数据目录仅允许一个写进程，重设密码与运行中的服务互斥。损坏或无效的状态文件会保留原文件并报错退出，不会自动清空账号。

环境变量见 [配置示例](<config.example.env>)：

| 变量 | 默认值 / 含义 |
| --- | --- |
| `BIND` / `PORT` | `127.0.0.1` / `8791` |
| `DATA_DIR` | `data` |
| `MEOO_AI_BASE` | Meoo OpenAI-compatible 网关地址 |
| `MEOO_PROJECT_AK` | 可选：初次预置一个项目 AK |
| `MEOO2API_KEYS` | 可选：初次预置客户端 API Key，逗号分隔 |
| `MEOO2API_ADMIN_PASSWORD` | 可选：初始管理密码 |
| `TZ` | 进程时区；容器/服务示例使用 `Asia/Shanghai` |

环境文件不会由二进制自动加载；可通过 systemd `EnvironmentFile=` 或启动环境注入。控制台配置保存即生效。默认输出预算为 0（跟随上游、不额外限制），请求中明确指定的预算优先。

### Docker

```bash
docker compose up -d --build
docker compose logs meoo2api
```

[Compose 配置](<docker-compose.yml>) 使用命名卷持久化数据，镜像以 UID 10001 的普通用户运行；自定义绑定已有数据目录时，先确保该 UID 可读写。镜像含 CA 证书与时区数据，构建上下文排除本地账号状态。

### systemd

使用 [用户服务示例](<meoo2api.service.example>)。默认监听地址为本机；按实际部署传入 `-bind`。公开控制台时，建议在已有反向代理上启用 HTTPS，避免通过明文 HTTP 传送密码和密钥。

## 持久化与账号调度

- 状态以权限 `0600` 写入数据目录；使用串行快照、临时文件、fsync 与原子替换，停止时取消任务并保存最终状态。
- 账号停用立即将全部关联项目 AK 排除；删除清理本地账号及关联凭证，不删除远端 Meoo 项目。
- 对限流/暂时不可用与凭证错误分别冷却；普通参数错误、用户取消不触发错误冷却。
- 保留历史未绑定项目 AK，并在账号管理中直接探活、启停或删除，不另设密钥池页面。
- 签到与保活在进程内调度，无需 cron。日期、签到小时按服务器进程时区计算。临时网络失败不标记凭证永久过期。
- 管理密码使用带随机盐 PBKDF2-SHA256；旧 SHA256 密码在成功登录后升级。管理会话有到期、注销和限流，管理 API 只接受同源浏览器请求。
- 「导出脱敏概况」不含凭证明文，也不是恢复备份。需要恢复时，在服务停止后备份完整数据目录并妥善保管。
- 运行日志输出至 stdout/stderr，systemd 部署通过 `journalctl --user -u meoo2api` 查看；未内置额外日志文件。

## 测试

```bash
./scripts/check.sh
# 或分开运行
go vet ./...
go test -race -count=1 -timeout=90s ./...
node --test internal/webui/console.test.cjs
```

Go 回归测试使用本地 mock 验证授权一次消费、账号去重/项目恢复、凭证轮换、并发保存、签到生命周期、网关协议转换、错误处理、管理接口、模型目录增删/缓存隔离以及长输出结束语义。Node 仅用于开发阶段的控制台测试，不是服务运行依赖。

## 目录

```text
cmd/server/         启动、路由、中间件与优雅退出
internal/model/     数据结构、配置边界、密码
internal/store/     原子持久化、目录锁与兼容迁移
internal/upstream/  HTTP 与上游错误处理
internal/catalog/   上游模型目录同步、缓存和故障回退
internal/pool/      账号调度、冷却与探活
internal/gateway/   Chat / legacy / Responses 适配
internal/admin/     控制台鉴权和管理 API
internal/account/   账号、设备授权、个人/项目凭证
internal/checkin/   签到、保活调度
internal/webui/     五页控制台，静态资源编入二进制
```

## 许可

MIT
