# meoo2api

把 Meoo 模型网关包成 OpenAI 兼容 API，带账号管理控制台。Go 单二进制。

```
OpenAI 客户端 → http://host:8791/v1 → https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 凭证链（实测结论）

| 凭证 | 获取方式 | 能力 |
|---|---|---|
| 设备授权 `meoo_ak_…` | `POST /api/v1/cli/auth/session` → 浏览器打开 `/cli-login?session_id=` 点授权 → 轮询取回 | 身份、项目、开云、积分 |
| 项目服务 `meoo_project_…` | 用上面的 key 建项目 → 开云 → 读 `/cloud/environment` | 调模型网关，**进号池的就是它** |
| 网页 Cookie | 浏览器登录 | 签到、会话保活 |

- 设备授权是 Meoo 网页端的 CLI 登录流程（Meoo CLI 自己用的是 OAuth PKCE + 本机回调，远端服务器收不到回调）
- 授权会话**只能读取一次**：第一次 `approved` 返回 `apiKey`，之后再查就是 `expired`
- 个人 AK 调模型网关固定 `401 SERVICE_AK_FORMAT_INVALID`；OAuth/个人 AK 调 `/api/mc/entitlement/init` 固定 `401 NEED_LOGIN`

## 添加账号

控制台「账号管理 → 添加账号 → 浏览器授权登录」：

1. 服务端创建授权会话，返回 `https://meoo.com/cli-login?session_id=<sid>`
2. 浏览器打开（已登录则点一下「授权」）
3. 服务端轮询一次拿到 `apiKey`、自动完成：识别账号 → 建项目 → 开云 → 取项目 Key → 入池 → 查积分

不需要命令行、不需要回调地址、不需要手动拷 Cookie。

Cookie 只在需要签到/保活时用，可在同一弹窗里单独粘贴。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 支持 stream / tools / n>1 |
| POST | `/v1/completions` | legacy |
| POST | `/v1/responses` | Responses API |
| GET | `/v1/models` | 模型列表 |
| GET | `/` | 控制台 |
| GET | `/healthz` | 健康检查 |

调用凭证二选一：控制台「API Key」页签发的 `sk-…`，或直接带账号的项目 AK（BYOK）。

## 运行

```bash
./scripts/build.sh
./meoo2api -bind 0.0.0.0 -port 8791 -data data
./meoo2api --reset-password          # 重设管理密码
docker compose up -d --build
```

`config.example.env`：`MEOO_PROJECT_AK`（可选预置）、`MEOO2API_ADMIN_PASSWORD`、`BIND`、`PORT`、`DATA_DIR`。

状态：`data/state.json`（600）；日志：`meoo2api.log`。

## 调度

进程内完成，不用 cron：签到每天一次（过了 `checkinHour`）；会话保活每 `keepAliveHours` 小时。

## 目录

```
cmd/server/        入口与路由装配
internal/model/    数据结构
internal/store/    状态持久化 + 旧结构迁移
internal/upstream/ Meoo 网关客户端
internal/pool/     Key 调度、探活、冷却
internal/gateway/  OpenAI 兼容面
internal/admin/    控制台 API
internal/account/  账号：设备授权、开通项目、积分、保活
internal/checkin/  签到与保活调度
internal/webui/    控制台（embed）
```

## 许可

MIT
