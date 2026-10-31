# dspool

本地运行的 DeepSeek API key 号池代理。给 Claude Code、Codex CLI、pi 以及其他 OpenAI / Anthropic 兼容客户端使用。

多个 DeepSeek key 会被自动探活、按余额和负载路由；key 被吊销会自动移出，余额不足会进冷池，充值后自动回热。客户端只需要指向一个本地地址，不需要感知 key 的变化。

```
Claude Code ─┐
Codex CLI  ──┼──▶  dspool 127.0.0.1:8787  ──▶  api.deepseek.com
pi / SDK   ──┘         │
                       ├── /v1/messages          (Anthropic 透传)
                       ├── /v1/chat/completions  (OpenAI 透传)
                       ├── /v1/responses         (Responses → Chat 转换)
                       ├── 热池 / 冷池 / 已移除 / 冷却
                       ├── 余额轮询 + 自动恢复
                       └── 免鉴权 dashboard
```

## 特性

- **自动路由**：按策略在多个 key 之间分配请求，支持 in-flight 感知。
- **余额感知**：定期调用 DeepSeek `GET /user/balance`，余额不足的 key 进冷池，充值后自动回热。
- **吊销处理**：401/403 的 key 立即移出号池并落盘 tombstone，重启也不会复活；可在 dashboard 手动恢复。
- **无感失败转移**：401、402、429、5xx、网络错误都会换 key 重试；流式请求在开始向客户端输出前完成状态判断。
- **三种客户端协议**：
  - Claude Code / Anthropic SDK：`POST /v1/messages`、`POST /v1/messages/count_tokens`
  - OpenAI SDK / pi：`POST /v1/chat/completions`
  - Codex CLI / pi Responses：`POST /v1/responses`（把 Responses 转成 DeepSeek 的 Chat Completions，含流式事件、function tool、custom tool）
- **本地 dashboard**：余额、状态、并发、错误、冷池情况一页看完，可手动刷新余额、恢复被移除的 key。
- **单文件部署**：Go 单二进制，无数据库；状态只有一个可选的 JSON 文件。

## 设计说明

### key 状态机

```
            probe 成功且余额可用
   probing ───────────────────────▶ hot ── 402 ──▶ cold
      │                              │              │
      │ probe 401/403                │ 401          │ probe 发现余额恢复
      ▼                              ▼              ▼
   invalid（移出号池，tombstone） ◀───┘           hot
```

- **hot**：可接流量。429 / 5xx / 网络错误只会让它进入短暂 `cooldown`，不改变池子归属。
- **cold**：余额不足（402 或余额检查发现 `is_available=false` / 余额为 0）。冷池按 `cold_check_interval` 定时复查，充值后自动回热；没有可用热 key 时还会额外同步刷新一次冷池。
- **invalid**：key 被吊销。立即移出号池，并写进 `state.json` 的 tombstone。恢复需要通过 dashboard 的“恢复”按钮或 `/admin/keys/{id}/restore`。
- **probing**：启动时或刚被恢复，正在等待余额检查。

### 路由策略

`pool.strategy` 可选：

| 策略 | 行为 | 适合 |
| --- | --- | --- |
| `spread`（默认） | 选当前 in-flight 最少的 key；同负载时优先余额小的 key | 这些 key 也在被别人使用、想要最大本地吞吐和余额利用率，并先把零碎余额花掉 |
| `weighted` | 随机，权重 = 余额 / (1 + in-flight × penalty) | 想优先消耗余额大的 key、降低 402 概率 |
| `highest` | 总是选余额最高的 key | 尽量拉长可用时间 |
| `drain` | 优先消耗余额最小的 key | 把零碎余额尽快用掉，避免被别人先花完 |
| `round_robin` | 按配置顺序轮询 | 简单可预测 |

**为什么默认是 `spread`：** 这些 key 同时也会被别人使用，且 DeepSeek 对单 key 有并发/QPS 限制。把当前最空闲的 key 用起来，可以把聚合吞吐做大；同负载时优先余额小的 key，相当于“先花掉容易被别人抢先花完的零碎余额”。某个 key 被外部流量打到 429 时，冷却它并换下一个 key 即可，不会成为整体瓶颈。余额决定 key 在不在热池、同负载时的选择，以及 402 后的冷池处理。

可调参数：

- `max_attempts`：一个客户端请求最多试几个上游 key。
- `max_inflight_per_key`：每个 key 的本地并发上限，0 不限制。
- `inflight_penalty`：`weighted` 策略下 in-flight 的权重惩罚。
- `min_balance`：余额低于该值的 key 直接放冷池（默认 0，即只要 > 0 就可用）。

### 无感失败转移

每个请求最多尝试 `max_attempts` 个 key（不会重复同一个 key）：

| 上游结果 | 处理 |
| --- | --- |
| 2xx | 正常转发 |
| 401 / 403 | key 标记 `invalid`，写 tombstone，立刻换 key |
| 402 Insufficient Balance | key 进冷池，立刻换 key |
| 429 | 按 `Retry-After` / `rate_limit_cooldown` 冷却，换 key |
| 5xx / 网络错误 | 短冷却，换 key |
| 其他 4xx | 不重试，原样返回给客户端（通常是请求本身的问题） |

流式请求会先等待上游响应头：只有拿到 2xx 才开始向客户端写 SSE；因此 401/402/429/5xx 的换 key 对客户端是透明的。已经开始输出后如果上游断流，则无法重放，只能结束该次流。

### 余额轮询

- 热池：每 `balance_interval`（默认 30s）检查一次。
- 冷池：每 `cold_check_interval`（默认 2m）检查一次，等待充值/恢复。
- 可用 `POST /dashboard/refresh` 或 `POST /admin/refresh` 立即检查全部 key。
- 余额接口不可达不会误删 key；启动时会乐观放行，后续真实请求再决定。

## 快速开始

需要 Go 1.27+。

```bash
cp config.example.yaml config.yaml
cp keys.txt.example keys.txt
$EDITOR keys.txt          # 填入你的 DeepSeek key

make build
./dspool -config config.yaml
```

打开 <http://127.0.0.1:8787/> 查看 dashboard。

也可以不用配置文件：

```bash
DSPOOL_KEYS="sk-key-1,sk-key-2" ./dspool
```

或者把 key 直接写在 `config.yaml`：

```yaml
keys:
  - label: main
    key: sk-xxxx
  - sk-yyyy
```

`config.yaml`、`keys.txt`、`data/` 默认被 `.gitignore` 忽略。

### 安装到 PATH

把二进制链接到已经在 PATH 里的目录（例如 `~/.local/bin`）：

```bash
ln -sfn /Users/pluto/dspool/dspool ~/.local/bin/dspool
dspool --version
```

之后可以在任意目录使用：

```bash
dspool claude
dspool codex
dspool pi
```

找不到 `./config.yaml` 时，dspool 会依次尝试：

1. `~/.config/dspool/config.yaml`
2. `~/.dspool/config.yaml`
3. 二进制所在目录（会解析软链接）的 `config.yaml`

所以上面的软链接会自动使用 `/Users/pluto/dspool/config.yaml`；要切换配置，显式传 `-config /path/to/config.yaml` 即可。


## 快捷命令

不想手写环境变量时，可以直接用 dspool 启动 agent：

```bash
dspool claude --resume
dspool codex exec "fix the tests"
dspool pi -p "hello"
```

行为：

- 如果配置的监听地址上已经有 dspool 在跑，就直接复用；否则自动启动一个内嵌代理，agent 退出后关闭。想强制使用已经常驻的代理，加 `-no-embed`。
- 子命令后面的所有参数原样透传，agent 的退出码也会原样返回。
- 全局 flag 必须放在子命令前：`dspool -config other.yaml claude --resume`。
- 自动配置：
  - **Claude Code**：注入 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_MODEL` 等；已有模型环境变量不覆盖；清掉 `CLAUDE_CODE_OAUTH_TOKEN`、Bedrock/Vertex 开关，避免绕开代理。
  - **Codex**：通过 `-c` 注入 `dspool` provider、`DSPOOL_API_KEY`、`wire_api=responses` 和 `features.respect_system_proxy=true`，不需要改 `~/.codex/config.toml`。
  - **pi**：自动写入/更新 `$PI_CODING_AGENT_DIR/models.json`（默认 `~/.pi/agent/models.json`）里的 `providers.dspool`，并注入 `--provider dspool --model ... --api-key ...`；其他 provider 保留。`pi --help`、`pi install` 等子命令不会改配置，也不会注入模型参数。
- 客户端 token 使用 `client_keys` 第一项；没配置时用 `dspool-local`（代理此时接受任意 token）。
- 如果机器设置了 `HTTP_PROXY`/`HTTPS_PROXY`，快捷命令会给 agent 追加本地 `NO_PROXY=127.0.0.1,localhost`，避免本地请求被送到前向代理；Codex 还会开启按目标解析系统代理。
- 内嵌代理只在这个 agent 进程存活期间运行。要常驻、给多个客户端共享，先 `dspool serve`，然后在各终端用 `dspool -no-embed claude` / `dspool -no-embed codex` / `dspool -no-embed pi`。如果直接用快捷命令同时开多个 agent，第一个退出时会关闭它启动的内嵌代理。
- 快捷命令默认把 dspool 自己的日志写到 `<state_file 所在目录>/dspool-agent.log`（默认 `data/dspool-agent.log`），不会混进 Claude Code / Codex / pi 的终端界面。可以用 `log_file` 或 `DSPOOL_LOG_FILE` 改路径；`dspool serve` 默认仍输出到 stderr。
- `dspool pi` 自动维护配置要求 `models.json` 是严格 JSON；如果文件里有注释导致解析失败，它会报错而不会覆盖文件。可以手动按下面的 pi 示例添加 provider，或先去掉注释。

## 客户端接入

### Claude Code

最省事的方式是 `dspool claude`（见上一节）。手动配置如下：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=dspool-local          # 与 client_keys 一致；留空则任意 token
export ANTHROPIC_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_OPUS_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_SONNET_MODEL='deepseek-flash[1m]'
export ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-flash
export CLAUDE_CODE_SUBAGENT_MODEL=deepseek-flash
claude
```

也可以写进 `~/.claude/settings.json`：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ANTHROPIC_AUTH_TOKEN": "dspool-local",
    "ANTHROPIC_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "deepseek-flash[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "deepseek-flash",
    "CLAUDE_CODE_SUBAGENT_MODEL": "deepseek-flash"
  }
}
```

说明：

- `ANTHROPIC_BASE_URL` **不要**带 `/anthropic`，dspool 会把 `/v1/messages` 转发到 `https://api.deepseek.com/anthropic/v1/messages`。
- Claude Code 自带的 `?beta=true` 等 query 会原样保留。
- `[1m]` 是客户端标记，dspool 会在发往上游前剥掉；DeepSeek 当前模型本身就是 1M 上下文。
- 如果不设置 `ANTHROPIC_MODEL`，Claude Code 发来的 `claude-*` 名会被映射到 `models.default`（默认 `deepseek-flash`），避免意外走到更贵的 Pro 计费。想保留 Claude 名字的语义可以在 `models.map` 里显式配置。
- `/v1/messages/count_tokens` 由本地粗略估算返回，DeepSeek 没有 tokenizer 接口。

### Codex CLI

最省事的方式是 `dspool codex`，它会通过 `-c` 自动注入 provider，不需要手写下面的配置。

Codex 现在只支持 Responses API。dspool 会把 `/v1/responses` 转成 DeepSeek 的 Chat Completions，并把流式事件翻译回去。

`~/.codex/config.toml`（provider 配置只支持用户级配置，不要写进项目级 `.codex/`）：

```toml
model = "deepseek-flash"
model_provider = "dspool"
model_reasoning_effort = "high"

[model_providers.dspool]
name = "dspool"
base_url = "http://127.0.0.1:8787/v1"
env_key = "DSPOOL_API_KEY"
wire_api = "responses"
```

```bash
export DSPOOL_API_KEY=dspool-local
```

注意：

- `base_url` 带 `/v1`，Codex 会在其后追加 `/responses`，所以最终是 `http://127.0.0.1:8787/v1/responses`。
- provider id 不能用 `openai`、`ollama`、`lmstudio` 等保留名。
- `model_reasoning_effort` 会被映射到 DeepSeek thinking：`none` 关闭思考，`low` 对应低，`medium`/`high` 对应 `high`，`xhigh`/`max` 对应 `max`。也可以用 `responses.thinking: enabled|disabled` 强制。
- Codex 的 `apply_patch` 是 freeform/custom tool；dspool 会把它桥接成 Chat function tool，再把模型返回的 JSON `{"input": "..."}` 还原成 `custom_tool_call`。namespace 形式的工具做尽力转换；服务端工具（web_search、file_search 等）不支持。
- `previous_response_id` 不支持。Codex 默认无状态、每轮携带完整 input，因此不受影响；其他客户端应发送完整对话。
- 结构化输出（`text.format=json_schema`）会降级成 `response_format=json_object`，并把 schema 写进 system prompt。DeepSeek Chat API 只支持 `text` / `json_object`。

### pi

最省事的方式是 `dspool pi`，它会自动维护 `models.json` 里的 `providers.dspool` 并注入 `--provider/--model/--api-key`。

手动配置 `~/.pi/agent/models.json`（OpenAI Chat Completions 方式）：

```json
{
  "$schema": "https://pi.dev/schemas/models.schema.json",
  "providers": {
    "dspool": {
      "name": "dspool (DeepSeek pool)",
      "baseUrl": "http://127.0.0.1:8787/v1",
      "api": "openai-completions",
      "apiKey": "dspool-local",
      "authHeader": true,
      "compat": {
        "supportsDeveloperRole": false,
        "supportsReasoningEffort": true,
        "supportsStore": false,
        "maxTokensField": "max_tokens",
        "thinkingFormat": "deepseek"
      },
      "models": [
        {
          "id": "deepseek-flash",
          "name": "DeepSeek Flash",
          "reasoning": true,
          "input": ["text", "image"],
          "contextWindow": 1000000,
          "maxTokens": 65536,
          "cost": { "input": 0.15, "output": 0.6, "cacheRead": 0.003, "cacheWrite": 0.15 }
        }
      ]
    }
  }
}
```

然后 `pi --provider dspool --model deepseek-flash`，或在 TUI 里 `/model` 选择。

pi 的 `api` 也可以设为：

- `openai-responses`：走 dspool 的 `/v1/responses` 转换层。
- `anthropic-messages`：`baseUrl` 设为 `http://127.0.0.1:8787`（不要 `/v1`），走 Anthropic 透传。

### 通用 OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="dspool-local")
resp = client.chat.completions.create(
    model="deepseek-flash",
    messages=[{"role": "user", "content": "hi"}],
)
```

## Dashboard

默认地址：<http://127.0.0.1:8787/>（`/dashboard` 也可以）。

- 概览：热池、冷池、已移除、冷却中、探活中、总数。
- 每个 key：状态、掩码、余额、in-flight、最近/下次检查、成功/尝试次数、最近错误。
- 自动每 5 秒刷新，也可以点“刷新余额”立即探活。
- 已移除的 key 可以直接点“恢复”，清除 tombstone 并重新探活。
- 免鉴权；只展示掩码后的 key，不返回完整 secret。

Dashboard 相关端点：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/`、`/dashboard` | HTML 页面 |
| GET | `/dashboard/data` | 状态 JSON |
| POST | `/dashboard/refresh` | 触发全量余额检查 |
| POST | `/dashboard/keys/{id}/restore` | 恢复 key |

> dashboard 没有鉴权。默认只监听 `127.0.0.1`；如果要把监听地址改到公网，请自己加反向代理鉴权/防火墙，否则会暴露余额、掩码 key 和手动恢复能力。

代理默认也不返回 CORS 头，并会拒绝带跨站 `Origin` 或 `Sec-Fetch-Site: cross-site` 的浏览器请求，避免恶意网页通过浏览器消耗本地号池。`client_keys` 为空时，只接受 `Host` 为 loopback / localhost 的请求（防 DNS rebinding）；如果要在局域网通过 IP 访问，请配置 `client_keys`。

## 管理接口

设置 `admin_token` 后，`/admin/*` 需要 `Authorization: Bearer <admin_token>` 或 `X-Admin-Token`。未设置时只允许 loopback 访问。

```bash
# 完整状态（含 tombstone）
curl -s http://127.0.0.1:8787/admin/status | jq

# 立即检查全部 key 余额
curl -s -X POST http://127.0.0.1:8787/admin/refresh | jq

# 恢复被移除的 key（id 从状态里取）
curl -s -X POST http://127.0.0.1:8787/admin/keys/<id>/restore | jq

# 设置了 admin_token 时
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:8787/admin/status | jq
```

健康检查：

```bash
curl -s http://127.0.0.1:8787/healthz
# 有热 key 时 200，否则 503
```

## 配置参考

所有时长使用 Go duration 格式（`30s`、`2m`、`1h`），也可以写数字表示秒。

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8787` | HTTP 监听地址 |
| `state_file` | `data/state.json` | tombstone 持久化文件；相对路径基于配置文件目录 |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `log_file` | `""` | 日志文件；空时 `serve` 写 stderr，agent 快捷命令写 `<state_file 目录>/dspool-agent.log` |
| `client_keys` | `[]` | 客户端 token 列表；空 = 接受任意 token |
| `admin_token` | `""` | `/admin/*` token；空时只允许 loopback |
| `max_body_bytes` | `67108864` | 请求体上限（64 MiB） |
| `upstream.anthropic_base` | `https://api.deepseek.com/anthropic` | Anthropic 兼容 base |
| `upstream.openai_base` | `https://api.deepseek.com/v1` | OpenAI 兼容 base |
| `upstream.balance_url` | `https://api.deepseek.com/user/balance` | 余额接口 |
| `upstream.request_header_timeout` | `180s` | 等待上游响应头的超时 |
| `upstream.probe_timeout` | `15s` | 单次余额检查超时 |
| `upstream.max_tokens_cap` | `0` | 强制截断 `max_tokens`；0 不截断 |
| `upstream.anthropic_version` | `2023-06-01` | 客户端未带 `anthropic-version` 时补上 |
| `pool.strategy` | `spread` | 路由策略 |
| `pool.max_attempts` | `4` | 单请求最多尝试的 key 数 |
| `pool.max_inflight_per_key` | `0` | 每 key 本地并发上限；0 不限制 |
| `pool.inflight_penalty` | `1.0` | `weighted` 的 in-flight 惩罚 |
| `pool.min_balance` | `0` | 低于该余额进冷池 |
| `pool.balance_interval` | `30s` | 热池余额检查间隔 |
| `pool.cold_check_interval` | `2m` | 冷池检查间隔 |
| `pool.startup_probe_timeout` | `30s` | 启动探活阻塞上限 |
| `pool.rate_limit_cooldown` | `30s` | 429 最小冷却时间（Retry-After 更长时采用 Retry-After） |
| `pool.error_cooldown` | `10s` | 5xx / 网络错误冷却 |
| `pool.refresh_cold_on_exhaustion` | `true` | 热池耗尽时同步检查冷池 |
| `pool.exhaustion_refresh_cooldown` | `10s` | 上述同步检查的限流间隔 |
| `models.default` | `deepseek-flash` | 非 DeepSeek 模型名的默认映射 |
| `models.small` | `deepseek-flash` | 带 `haiku` 的模型名映射 |
| `models.map` | `{}` | 精确名或 glob 模式映射 |
| `responses.thinking` | `auto` | Responses 请求的 thinking 策略：`auto` / `enabled` / `disabled` |
| `keys_file` | `""` | key 文件，每行一个；支持 `label = key` |
| `keys` | `[]` | 内联 key 列表 |

环境变量覆盖：`DSPOOL_LISTEN`、`DSPOOL_STATE_FILE`、`DSPOOL_LOG_LEVEL`、`DSPOOL_LOG_FILE`、`DSPOOL_ADMIN_TOKEN`、`DSPOOL_STRATEGY`、`DSPOOL_KEYS`、`DSPOOL_KEYS_FILE`、`DSPOOL_CLIENT_KEYS`。

### key 文件格式

```
# 注释
main = sk-aaaaaaaa
backup = sk-bbbbbbbb
# 不带 label 也可以
sk-cccccccc
```

## 状态与持久化

`state.json` 只保存被 DeepSeek 拒绝过的 key tombstone：

```json
{
  "version": 1,
  "invalid": {
    "a1b2c3d4e5f60718": {
      "label": "backup",
      "mask": "sk-…abcd",
      "reason": "api key rejected by DeepSeek: HTTP 401: …",
      "since": "2026-10-31T12:00:00Z"
    }
  }
}
```

- 不保存 key 明文，只保存 SHA-256 指纹、label、掩码和原因。
- 文件权限 0600，原子写入。
- 冷池状态不持久化；重启后会重新探活，按真实余额回到热池或冷池。
- 删除 `state.json` 会清空所有 tombstone，让被移除的 key 重新参与探活。

发送 `SIGHUP` 会重新读取配置里的 key 和 `keys_file` 并热更新（新增/删除）；其他配置项需要重启。

## 部署

### systemd

仓库里有 `deploy/dspool.service` 示例：

```bash
sudo useradd --system --home /var/lib/dspool --shell /usr/sbin/nologin dspool
sudo mkdir -p /etc/dspool /var/lib/dspool
sudo cp config.example.yaml /etc/dspool/config.yaml
sudo cp keys.txt.example /etc/dspool/keys.txt
sudo chown -R dspool:dspool /var/lib/dspool
sudo cp deploy/dspool.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now dspool
sudo systemctl reload dspool   # SIGHUP，热加载 key
```

### launchd（macOS）

仓库里有 `deploy/com.dspool.plist` 示例。替换其中的用户名和路径后：

```bash
cp deploy/com.dspool.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.dspool.plist
```

### Docker

```bash
docker build -t dspool .

docker run -d --name dspool \
  -p 127.0.0.1:8787:8787 \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  -v "$PWD/keys.txt:/config/keys.txt:ro" \
  -v "$PWD/data:/data" \
  dspool
```

容器里需要把 `listen` 改成 `0.0.0.0:8787`，并设置 `client_keys`；否则请把端口只映射到 `127.0.0.1`。`state_file` 建议设为 `/data/state.json`。

## 模型说明

截至 2026-10，DeepSeek 的模型名是：

- `deepseek-flash`：便宜、快，1M 上下文，默认 thinking。
- `deepseek-v4-pro`：更强、更贵，1M 上下文。

旧的 `deepseek-chat` / `deepseek-reasoner` 已下线。dspool 不做硬编码校验，直接透传 `deepseek-*` 名称；如果 DeepSeek 再改名字，可以在 `models.default` / `models.map` 里调整。

- `claude-*`、`gpt-*` 等非 DeepSeek 名称默认映射到 `models.default`。
- 带 `haiku` 的名称（Claude Code 的后台小任务）优先映射到 `models.small`。
- `models.map` 支持精确名和 glob，例如：

```yaml
models:
  default: deepseek-flash
  small: deepseek-flash
  map:
    "claude-opus*": deepseek-v4-pro
    "claude-sonnet*": deepseek-flash
```

## 开发

```bash
make fmt
make vet
make test
make race
make build
```

目录：

```
cmd/dspool/            启动、信号、热加载
internal/config/       YAML + env 配置
internal/balance/      DeepSeek 余额接口
internal/pool/         状态机、路由策略、tombstone
internal/monitor/      定时探活、冷池恢复
internal/state/        state.json 原子持久化
internal/proxy/        HTTP 代理、Responses 转换、dashboard
```

测试覆盖了余额接口解析、冷热/失效状态迁移、各路由策略、并发上限、限流冷却、监控探活、配置合并、三种协议的端到端转发、Responses 流式转换（含 apply_patch custom tool）、dashboard 和鉴权。

## 已知限制

- 流式请求一旦开始向客户端输出就无法重放；失败转移只发生在首字节之前。
- 非流式响应会完整读入内存（上限 128 MiB）。
- `/v1/responses` 是翻译层，不是完整实现：
  - `previous_response_id` 不支持（会返回 400，客户端需要发送完整 input）。
  - 服务端内置工具（web_search、file_search 等）不支持；namespace 工具、custom tool、结构化输出是尽力转换。
  - Codex 的 `agent_message`、`compaction`、`local_shell_call` 等特殊历史项会被忽略；多 agent/压缩场景可能丢失部分上下文。
  - thinking 模式下 DeepSeek 不接受 `tool_choice: required` 或指定工具，代理会降级为 `auto`。
  - reasoning 内容不会被翻译成 Responses 的 reasoning summary。
- `dspool pi` 会在 `models.json` 里维护 `providers.dspool`；如果该文件不是严格 JSON（例如带注释），自动维护会报错并保持原文件不变。
- 快捷命令依赖 agent 对 `NO_PROXY` 的支持；Codex 额外启用 `features.respect_system_proxy=true`。如果某个 agent 版本仍然把 localhost 请求送进前向代理，可以去掉代理环境变量后再启动（或使用 `-no-embed` 配合自己控制的环境）。
- `count_tokens` 是本地估算，不是精确 tokenizer。
- 没有内建 Prometheus metrics；需要的话看 `/admin/status` 或 dashboard。
- 余额轮询会按 key 数产生请求（默认每个热 key 每 30 秒一次），key 特别多时可以调大 `balance_interval`。
