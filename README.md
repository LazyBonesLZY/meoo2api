# meoo2api

把 Meoo 模型网关包装成 OpenAI 兼容 API，自带管理控制台。**Go 实现，单二进制，零外部依赖**。

```
浏览器 / OpenAI 客户端 → http://127.0.0.1:8791 → https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 两种用法

**只用一把 key（BYOK）**：启动不需要任何配置，客户端 `Authorization: Bearer meoo_project_…` 直接用，
服务端不保存这把 key、不跨请求复用。

```bash
./meoo2api                        # 什么都不用配
open http://127.0.0.1:8791/       # 控制台里粘一把项目 AK 就能测
```

**固定 key**：`MEOO2API_KEYS` 是你的客户端 key，`MEOO_PROJECT_AK` 是服务端统一持有的项目 AK。

早期 Node 实现保留在 `nodejs/`，功能与 Go 版一致。

## 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` 、`/webui.html` | 测试用 WebUI |
| GET | `/healthz` | 健康检查，含上游探测 |
| GET | `/v1/models` | 模型列表（标准 list 信封） |
| GET | `/v1/models/{id}` | 单个模型 |
| POST | `/v1/chat/completions` | 支持 stream / tools / json_object / n>1 |
| POST | `/v1/completions` | legacy 文本补全 |
| POST | `/v1/responses` | Responses API 最小实现 |
| POST | `/v1/embeddings` 等 | 上游无此能力，返回 404 `unsupported_endpoint` |

## 构建与运行

```bash
git clone https://github.com/LazyBonesLZY/meoo2api.git && cd meoo2api
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o meoo2api .
./meoo2api                       # BYOK 模式，无需配置
./meoo2api -port 8791 -bind 0.0.0.0 -data /var/lib/meoo2api
```

管理密码在**首次启动时打印一次**（只存 SHA-256，不落盘）；`--reset-password` 重新生成。

WebUI 用 `go:embed` 打进二进制，不需要额外静态文件。

## 目录

```
main.go state.go pool.go upstream.go openai.go admin.go checkin.go   Go 服务端
webui.html                                                         控制台（embed）
login.mjs                                                          浏览器登录取 Cookie
nodejs/                                                            早期 Node 实现（保留）
```

```bash
curl http://127.0.0.1:8791/v1/chat/completions \
  -H 'Authorization: Bearer sk-xxx' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek v4.1 flash","messages":[{"role":"user","content":"你好"}],"max_tokens":600}'
```

```python
from openai import OpenAI
c = OpenAI(base_url="http://127.0.0.1:8791/v1", api_key="sk-xxx")
c.chat.completions.create(model="deepseek v4.1 flash",
    messages=[{"role": "user", "content": "你好"}], max_tokens=600)
```

## 环境变量

| 变量 | 说明 |
|---|---|
| `MEOO_PROJECT_AK` | 可选，服务端统一持有的项目 AK；不填则只能 BYOK |
| `MEOO2API_KEYS` | 可选，客户端 key（逗号分隔），命中后使用服务端的项目 AK |
| `MEOO_AI_BASE` | 上游地址，默认 `https://api.meoo.host/meoo-ai/compatible-mode/v1` |
| `PORT` / `BIND` | 默认 `8791` / `127.0.0.1` |
| `UPSTREAM_TIMEOUT` | 上游超时，默认 180000ms |
| `MAX_BODY_BYTES` | 请求体上限，默认 8MiB |
| `LOG` | 访问日志路径，默认同目录 `meoo2api.log` |
| `MEOO2API_ADMIN_PASSWORD` | 管理端密码；不设则首次启动随机生成 |

## 项目服务 AK

个人 AK（`meoo_ak_`）无法调用模型网关，需要项目服务 AK：

```bash
meoo login --ak meoo_ak_xxx
meoo projects create "my-project"     # 记录 urlId
meoo cloud enable                     # 开通云服务
curl -s https://meoo.com/open/v1/cli-compat/projects/<urlId>/cloud/environment \
  -H "Authorization: Bearer meoo_ak_xxx" | jq -r .data.meooProjectApiKey
```

## 控制台

`http://127.0.0.1:8791/`，需要管理密码：首次启动时**在终端打印一次**（不落盘，只存 SHA-256 哈希），
或用 `MEOO2API_ADMIN_PASSWORD` 指定。忘记密码：

```bash
systemctl --user stop meoo2api          # 必须先停，避免旧进程把 state 刷回去
node meoo2api.mjs --reset-password      # 打印一次新密码
systemctl --user start meoo2api
```

左侧导航 + 顶部状态，暗/亮主题。

| 视图 | 功能 |
|---|---|
| 概览 | 请求总量、成功率、P95、可用 key、近 24h 柱状图、告警条、一键全部探活 |
| 密钥池 | 点「+ 添加密钥」→ 只粘一把 `meoo_project_…` → 自动探活；备注、权重（1-5）、启停、解冷却、删除 |
| 访问密钥 | 写给调用方的 key：生成、复制、停用、删除 |
| 对话 | 流式/非流式、max_tokens、temperature、reasoning 折叠 |
| 工具调用 | 内置 `get_weather` / `get_time` / `calc`，自动 tool_calls → 本地执行 → 回传 → 最终回答 |
| 上下文隔离 | 连发两次独立请求，判定是否串味 |
| 每日签到 | 用 meoo.com 网页 Cookie 领每日登录积分，支持手动/全部/自动（默认每 6 小时），显示累计积分 |
| 模型 / 别名 | 上游模型清单、隐藏开关、别名增删 |
| 用量 | 按小时聚合：请求/成功/失败/prompt/completion/P50/P95 |
| 请求日志 | 服务端记录，按状态、request id、key 备注过滤 |
| 设置 | 默认 max_tokens、超时、粘性、软/硬冷却、日志上限、管理密码、导出 state |

### key 池行为

- 按权重轮询；开启「粘性」后优先复用上一个 key
- 上游 429/5xx → 软冷却（默认 60s）；401/403 → 硬冷却（默认 900s）
- 探活会真实请求上游 `/models`，鉴权失败标记 `invalid` 并进入硬冷却
- 没有可用 key 时返回 `503 no_available_key`；BYOK 请求不占池

## 账号模型（重要）

同一个"人"在 Meoo 侧有三个互不相等的标识，号池必须分清：

| 身份 | 形态 | 能干什么 | 能否自证账号 |
|---|---|---|---|
| 个人 AK | `meoo_ak_…`（48 字符） | `/open/v1/*` 开放接口、CLI | ✅ `/open/v1/cli-compat/account/identity` |
| 项目服务 AK | `meoo_project_…`（87 字符） | `/meoo-ai` 模型网关，**进池的就是它** | ❌ 只能被调用，查不到账号 |
| 网页登录态 | Cookie | `/api/v1/*`、`/api/mc/*`、签到、建 AK | ✅ `/api/v1/user/me` |

`/account/points` 返回的 `account.userId` 是**计费账户 id**，又是另一套编号（实测两个账号分别为
`1455987876734664` 和 `1932193098442650`），不要和 `user_id` 混用。

控制台的处理：

- 签到记录会写入识别出的账号（`user_id` + 名称/手机号脱敏）
- 项目服务 AK 无法自证账号，只有通过「登录态开项目」创建时才自动绑定
- 账号不一致时，每日签到页顶部会提示：**积分只归登录态账号，模型调用仍走池内账号**

### 从登录态自动开 Key / 开项目

每日签到行上的三个按钮：

| 按钮 | 实际调用 | 结果 |
|---|---|---|
| 识别 | `GET /api/v1/user/me` | 写入账号 `user_id` / 昵称 / 手机号 |
| 建 AK | `POST /api/v1/user/api-keys`（13 个 scope） | 生成 `meoo_ak_…`，可调 Open API，**不能进模型池** |
| 开项目 | 建项目 → `POST /{id}/cloud` → `GET /{id}/cloud/environment` | 产出 `meoo_project_…`，自动入池并绑定账号；**占 1 个云服务额度** |

「开项目」需要二次确认，会产生真实资源（PostgreSQL/Auth/Storage）。

### 自动签到

服务内置调度：每 30 分钟检查一次，进程启动 45 秒后先跑一轮。

- 条件：`autoCheckin=true` 且该账号 `lastGrantDate != 今天` 且距上次尝试超过 `checkinIntervalH`
- 结果 `ALREADY_GRANTED_TODAY` 时也会把 `lastGrantDate` 记为今天，避免一天内反复请求
- 跨天后的第一个 tick（≤30 分钟）会真正领取当日积分
- 控制台「每日签到」页显示：自动签到开关、间隔、上次自动运行时间

外部定时器（可选，和内置调度二选一）：

```bash
10 8 * * * cd ~/meoo2api && /usr/bin/node login.mjs --headless --api http://127.0.0.1:8791 --password '***' >> login.log 2>&1
```

## 每日签到

Meoo 的每日登录积分（`daily_login`）不是 AK 能拿的：网页端打开时调用
`GET /api/mc/entitlement/init`，响应里的 `dailyLoginGranted` / `dailyLoginGrantAmount` 就是当天发放结果。
用 AK 请求该接口会被 `API_KEY_ROUTE_FORBIDDEN` 挡掉，所以本项目的签到走**网页登录态 Cookie**。

在「每日签到」页粘贴 meoo.com 的 Cookie（F12 → Network → 任意请求的 Cookie 头），即可手动或自动领取。
Cookie 存在 `data/state.json`（600），接口返回时只显示前 8 位。

### 手机号登录（自动拿 Cookie）

Meoo 登录走阿里云 SSO（`account.aliyun.com/sso/login.htm`，client_id=oneday），
自己实现短信接口会撞 baxia 反爬，所以用浏览器代登：

```bash
node login.mjs                                   # 弹窗，手机号登录后自动抓 Cookie
node login.mjs --headless                        # 复用已登录的 profile，无需人工
node login.mjs --api http://127.0.0.1:8791 \
  --password <管理密码> --label 主号              # 抓完直接写入控制台并签到
```

流程：`/api/auth/login_redirect` 拿到 SSO 地址 → 浏览器授权 → 回跳 `meoo.com/aliyunLogin` 种 Cookie
→ CDP 读取并请求 `entitlement/init` 验证 → 写入签到账号。

profile 默认 `~/.meoo-login-profile`，登录一次后有效期由阿里云会话决定；
建议加个每日定时：

```bash
# crontab -e
10 8 * * * cd ~/meoo2api && /usr/bin/node login.mjs --headless --api http://127.0.0.1:8791 --password '***' >> login.log 2>&1
```

`dailyLoginGranted=false` + `dailyLoginSkipReason=ALREADY_GRANTED_TODAY` 表示当天已领过，
余额以 `/open/v1/cli-compat/account/points` 为准。

## 运行数据

```
data/state.json   key 池、客户端 key、签到账号、别名、配置、日志、用量（600）
meoo2api.log      访问日志 JSONL（不含 body、不含 key 明文）
```

管理密码只存哈希，不落盘；首次启动打印一次。

## 无状态与隔离

服务端**不保存任何会话状态**：每个请求的 `messages` 就是全部上下文，请求之间不共享记忆。

- 不写数据库、不写 cookie、不设 session 头；日志只记 method / path / status / 耗时，不记 body、不记 key
- BYOK 模式下每请求的 key 只在该请求内使用，用完即弃
- `n>1` 是同一个 payload 并发发起，不引入跨请求状态
- 实测：第 1 次 `prompt_tokens=73`（含暗号），第 2 次 `prompt_tokens=71`（只有它自己那条），模型回答"不知道"

## 上游差异说明

Meoo 网关是阿里系的 OpenAI 兼容实现，本层补齐了这些地方：

- `/models` 返回 `{"models":[…]}`，已转成标准 `{"object":"list","data":[…]}`。
- 上游只接受 `n=1`，`n>1` 由本层并发调用合并（不支持 `n>1` + `stream`）。
- `stream_options` 必须与 `stream:true` 成对出现，本层自动配对或剔除。
- 不支持 embedding / 语音 / 图像，统一返回 404 `unsupported_endpoint`（上游原生回 500）。
- thinking 模型（如 `deepseek-v4.1-flash-1017`）的 `max_tokens` 需 ≥600，否则正文为空、全被 reasoning 占用。

## 部署

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o meoo2api .
cp meoo2api.service.example ~/.config/systemd/user/meoo2api.service
systemctl --user daemon-reload
systemctl --user enable --now meoo2api
systemctl --user status meoo2api
```

跨架构交叉编译：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o meoo2api-arm64 .
```

服务默认只绑 `127.0.0.1`，跨机访问建议 `ssh -L 8791:127.0.0.1:8791 host`，或自行加反向代理鉴权。

## 许可

MIT
