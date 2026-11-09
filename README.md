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

## 项目服务 AK

个人 AK（`meoo_ak_`）无法调用模型网关，需要项目服务 AK：

```bash
meoo login --ak meoo_ak_xxx
meoo projects create "my-project"     # 记录 urlId
meoo cloud enable                     # 开通云服务
curl -s https://meoo.com/open/v1/cli-compat/projects/<urlId>/cloud/environment \
  -H "Authorization: Bearer meoo_ak_xxx" | jq -r .data.meooProjectApiKey
```

## WebUI

`http://127.0.0.1:8791/` 打开即用，只需要粘一把项目 AK（存 sessionStorage，仅当前标签页）。
控制台布局：左侧导航 + 顶部状态，暗/亮主题切换。

| 视图 | 作用 |
|---|---|
| 概览 | 上游状态、模型数、延迟，一键连通性测试（拉模型 + 最小对话） |
| 对话 | 流式/非流式、max_tokens、temperature，reasoning 折叠展示 |
| 工具调用 | 内置 `get_weather` / `get_time` / `calc`，自动跑 tool_calls → 本地执行 → 回传 → 最终回答，每轮 finish_reason 与原始 JSON 都可见 |
| 上下文隔离 | 连发两次独立请求，验证第二次不带第一次的内容 |
| 模型 | 原生 id + 别名表格，一键复制 |
| 请求日志 | 本页请求记录（浏览器内存）+ 最后一次原始请求/响应 |
| 设置 | Base URL、鉴权方式、清除本地 key |

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
