# meoo2api

Meoo 模型网关的 OpenAI 兼容 API + 账号控制台。Go 单二进制。

```
OpenAI 客户端 → http://host:8791/v1 → https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 两个列表

| 列表 | 是什么 | 用在哪 |
|---|---|---|
| **账号管理** | Meoo 账号：登录态 + 它名下的项目 API Key + 积分余额 | 上游：决定请求打到哪个 Meoo 账号 |
| **API Key** | meoo2api 自己签发的 key（`sk-…`） | 调用方拿它访问本服务，后端自动选可用账号 |

调用方用 API Key；也支持直接带账号的项目 AK（BYOK），两者都行。

## 账号

一个账号 = 一份网页登录态 + 一个项目 API Key（`meoo_project_…`）。这个 API Key 就是调用 `meoo2api` 的凭证，也是号池条目。

- 添加账号（粘贴浏览器 Cookie）后自动执行：识别账号 → 建个人 AK → 建项目 → 开云服务 → 取项目 API Key → 入池
- 账号列表显示：项目 API Key、健康/冷却、积分余额、今日是否已签到
- 冷却可手动「解除冷却」；「刷新积分」用该账号的个人 AK 查 `/account/points`
- 个人 AK（`meoo_ak_…`）只能调 Open API，调模型网关会被拒（`401 SERVICE_AK_FORMAT_INVALID`），所以不放进池
- 无需客户端 key、无需额外分组：直接 `Authorization: Bearer meoo_project_…`

## 运行

```bash
./scripts/build.sh
./meoo2api -bind 0.0.0.0 -port 8791 -data data
```

首次启动打印管理密码（只存 SHA-256，不落盘）：

```bash
./meoo2api --reset-password
```

Docker：

```bash
docker compose up -d --build
```

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI 兼容，支持 stream / tools / n>1 |
| POST | `/v1/completions` | legacy 补全 |
| POST | `/v1/responses` | Responses API |
| GET | `/v1/models` | 模型列表 |
| GET | `/` | 控制台（概览 / 账号管理 / 请求日志 / 设置） |
| GET | `/healthz` | 健康检查 |

```bash
curl http://127.0.0.1:8791/v1/chat/completions \
  -H 'Authorization: Bearer meoo_project_xxx' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek v4.1 flash","messages":[{"role":"user","content":"hi"}],"max_tokens":600}'
```

## 账号与签到

| 动作 | 频率 |
|---|---|
| 签到 | 每天一次，过了设定整点（默认 0 点）且当天未领才请求 |
| 会话保活 | 每 N 小时（默认 6）打 `/api/v1/user/me`，失败标记会话失效 |
| 探活 | 手动，验证项目 API Key 是否可用 |

调度在进程内，不需要 cron。

## 配置

`config.example.env`：

```ini
MEOO_PROJECT_AK=          # 可选，预置一个项目 AK
MEOO2API_ADMIN_PASSWORD=  # 可选，不设则首次启动随机
BIND=127.0.0.1
PORT=8791
DATA_DIR=data
```

状态与日志：`data/state.json`（600）、`meoo2api.log`。

## 目录

```
cmd/server/        入口
internal/model/    数据结构
internal/store/    状态持久化 + 旧结构迁移
internal/upstream/ Meoo 网关客户端
internal/pool/     API Key 调度与探活
internal/gateway/  OpenAI 兼容面
internal/admin/    控制台 API
internal/account/  账号：识别、签到、接入号池
internal/checkin/  签到与保活调度
internal/webui/    控制台页面（embed）
scripts/build.sh   Dockerfile / docker-compose.yml
login.mjs          浏览器登录取 Cookie
nodejs/            早期 Node 实现
```

## 许可

MIT
