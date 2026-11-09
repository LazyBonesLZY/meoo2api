# meoo2api

把 Meoo 模型网关包装成 OpenAI 兼容 API，单文件、零依赖。

```
OpenAI 客户端 → http://127.0.0.1:8791/v1 → https://api.meoo.host/meoo-ai/compatible-mode/v1
```

## 端点

| 方法 | 路径 | 说明 |
|---|---|---|
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
cp .env.example .env      # 填入项目服务 AK 和客户端 key
node meoo2api.mjs
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
| `MEOO_PROJECT_AK` | 必填，项目服务 AK（`meoo_project_…`） |
| `MEOO2API_KEYS` | 客户端 key，逗号分隔；留空则不校验 |
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
