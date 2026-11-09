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
cmd/server/           入口（flag / env / 路由装配）
internal/
  model/              共用数据结构
  store/              状态持久化（key 池 / 签到 / 日志 / 用量）
  upstream/           Meoo 网关客户端与错误映射
  pool/               key 调度：权重轮询、粘性、冷却、探活
  gateway/            OpenAI 兼容面（chat / completions / responses）
  admin/              控制台 API 与登录态
  account/            账号识别、个人 AK、项目 AK 开通
  checkin/            每日签到与调度
  webui/              控制台页面（go:embed）
scripts/build.sh      构建脚本
Dockerfile / docker-compose.yml
config.example.env    环境变量样例
login.mjs             浏览器登录取 Cookie
nodejs/               早期 Node 实现（保留）
```

构建：

```bash
./scripts/build.sh                       # 产出 bin/meoo2api
docker compose up -d --build             # 或用容器
```

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

### 自动签到与保活

调度在进程内，每 5 分钟检查一轮：

| 动作 | 频率 | 说明 |
|---|---|---|
| 签到 | 每天一次 | 过了 `checkinHour` 且当天未领才请求；返回 `ALREADY_GRANTED_TODAY` 也记当天，不再重试 |
| 保活 | 每 `keepAliveHours` 小时 | `GET /api/v1/user/me`，维持登录态；失败标记 `expired` |
| 手动 | 随时 | 「签到」「保活」按钮，或 `POST /admin/api/checkins/{id}/run|ping` |

控制台显示：自动签到开关、领取整点、保活间隔、上次自动运行、每个账号的保活时间与是否失效。

