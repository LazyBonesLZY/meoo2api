# meoo2api

把 Meoo 模型网关包装成 OpenAI 兼容 API，自带测试用 WebUI，零依赖。

```
浏览器 / OpenAI 客户端 → http://127.0.0.1:8791 → https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 两种用法

**只用一把 key（BYOK）**：启动不需要任何配置，客户端 `Authorization: Bearer meoo_project_…` 直接用，
服务端不保存这把 key、不跨请求复用。

```bash
node meoo2api.mjs                 # 什么都不用配
open http://127.0.0.1:8791/       # WebUI 里粘一把项目 AK 就能测
```

**固定 key**：`MEOO2API_KEYS` 是你的客户端 key，`MEOO_PROJECT_AK` 是服务端统一持有的项目 AK。

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

## 快速开始

```bash
git clone https://github.com/LazyBonesLZY/meoo2api.git && cd meoo2api
node meoo2api.mjs          # BYOK 模式，无需配置
# 或固定 key：cp .env.example .env 后填 MEOO_PROJECT_AK / MEOO2API_KEYS
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

`http://127.0.0.1:8791/`，需要管理密码（首次启动随机生成，写入 `data/admin-password.txt`，
或用 `MEOO2API_ADMIN_PASSWORD` 指定）。左侧导航 + 顶部状态，暗/亮主题。

| 视图 | 功能 |
|---|---|
| 概览 | 请求总量、成功率、P95、可用 key、近 24h 柱状图、告警条、一键全部探活 |
| 密钥池 | 多个项目 AK：备注、权重（1-5）、启用/停用、单个探活、解除冷却、删除；显示健康状态与冷却倒计时 |
| 访问密钥 | 写给调用方的 key：生成、复制、停用、删除 |
| 对话 | 流式/非流式、max_tokens、temperature、reasoning 折叠 |
| 工具调用 | 内置 `get_weather` / `get_time` / `calc`，自动 tool_calls → 本地执行 → 回传 → 最终回答 |
| 上下文隔离 | 连发两次独立请求，判定是否串味 |
| 模型 / 别名 | 上游模型清单、隐藏开关、别名增删 |
| 用量 | 按小时聚合：请求/成功/失败/prompt/completion/P50/P95 |
| 请求日志 | 服务端记录，按状态、request id、key 备注过滤 |
| 设置 | 默认 max_tokens、超时、粘性、软/硬冷却、日志上限、管理密码、导出 state |

### key 池行为

- 按权重轮询；开启「粘性」后优先复用上一个 key
- 上游 429/5xx → 软冷却（默认 60s）；401/403 → 硬冷却（默认 900s）
- 探活会真实请求上游 `/models`，鉴权失败标记 `invalid` 并进入硬冷却
- 没有可用 key 时返回 `503 no_available_key`；BYOK 请求不占池

## 运行数据

```
data/state.json           key 池、客户端 key、别名、配置、日志、用量（600）
data/admin-password.txt   首次启动生成的管理密码（600）
meoo2api.log              访问日志 JSONL（不含 body、不含 key 明文）
```

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
cp meoo2api.service.example ~/.config/systemd/user/meoo2api.service
systemctl --user daemon-reload
systemctl --user enable --now meoo2api
```

服务默认只绑 `127.0.0.1`，跨机访问建议 `ssh -L 8791:127.0.0.1:8791 host`，或自行加反向代理鉴权。

## 许可

MIT
