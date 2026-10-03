# CLIProxyAPI-Plugins

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（下称 **CPA**）的插件工程。

> 本仓库存放 **CPA 插件**——由 CPA 在运行时通过插件宿主加载的 Go 动态库
> （`-buildmode=c-shared`）。这里既不是独立代理，也不是 CPA 的 fork；只有放进 CPA 部署里
> 代码才会生效。

| 插件 | ID | 作用 |
|---|---|---|
| **codex-policy** | `codex-policy` | 官方客户端准入、思考等级降级、客户端无感模型映射 |

`codex-policy` 工作在 CPA 的请求路径上，做三件事：

1. **官方客户端准入**：只允许官方 Codex 客户端访问，其他客户端返回
   `403 客户端不支持访问`。
2. **思考等级降级**：把 `xhigh` / `max` / `ultra` 改写为 `medium`，客户端无感。
3. **模型映射**：客户端请求模型 A，实际转发到模型 B，响应里再把模型名改回 A，客户端无感。

三者都支持**按 key 豁免**：命中的 key 既跳过准入，也不做改写。

当前版本：**0.5.0**

---

## 目录结构

```
CLIProxyAPIPlugins/
├── codex-policy/          插件源码（main.go / config_fields.go / config_patch.go / pages.go ...）
├── dist/codex-policy.so   已编译好的插件二进制
├── config-example.yaml    plugins.configs.codex-policy 配置样例
├── build.sh               构建脚本
├── README.md              英文文档
└── README_cn.md           本文件
```

## 三个 CPA 钩子的分工

模型映射**必须**走 ModelRouter 钩子。请求拦截器执行得太晚：执行器会用已解析好的模型覆盖
请求体的 `model` 字段（`helps.SetStringIfDifferent(body, "model", baseModel)`），
body 改写会被丢弃。

| 能力 | 钩子 | 时机 | 用途 |
|---|---|---|---|
| 模型映射 | `model.route` | 模型→provider 解析**之前** | 替换上游模型 |
| 准入 + 思考降级 | `request.intercept_before` | 凭据选择之前 | 拦客户端、降思考等级 |
| 还原可见模型名 | `response.intercept_after`、`response.intercept_stream_chunk` | 回给客户端之前 | 把模型 A 报给客户端 |

## 构建与部署

```bash
cd CLIProxyAPIPlugins
./build.sh

# plugins/ 一般是 bind mount，直接 cp 即可
cp dist/codex-policy.so ../plugins/linux/amd64/codex-policy.so
docker compose restart cli-proxy-api
```

CPA 日志出现下面这行即加载成功：

```
pluginhost: plugin registered plugin_id=codex-policy plugin_name=codex-policy version=0.5.0
```

两个常见坑：

1. `plugins.configs.codex-policy.enabled: false` 会让 CPA **完全跳过加载且不打任何日志**，
   这是「插件消失」最常见的原因。CPA 把缺省的 `enabled` 当作 `false`，所以必须显式写。
2. `model_map.provider` 默认留空或填 `auto`，CPA 按目标模型自动选择可用供应商。
   只有需要固定渠道时才填写注册表 key，例如 `openai-compatible-subapi`；显式指定已禁用渠道会记录 `PROVIDER_UNAVAILABLE`。

## 配置（CPA 的 `config.yaml`）

```yaml
plugins:
  enabled: true
  configs:
    codex-policy:
      enabled: true
      enforce_official_client: true
      reject_status: 403
      reject_message: "客户端不支持访问"
      official_min_score: 5          # 需命中 >=1 个强信号，且总分 >= 该值
      enforce_all_protocols: true    # 准入覆盖所有协议（推荐）
      apply_to_non_codex: false      # 「改写」是否也作用于非 Responses 流量
      exempt_api_keys: []            # 这些 key 不限制
      exempt_bypass_admission: true
      effort:
        enabled: true
        downgrade_from: [xhigh, max, ultra]
        target: medium
        models: []                   # 空 = 不限模型
        skip_for_exempt_keys: true
      model_map:
        enabled: true
        provider: ""                    # 默认自动选择供应商，可写 auto
        preserve_thinking_suffix: true     # 保留 model(xhigh) 的后缀
        rewrite_response_model: true       # 响应里报客户端请求的模型名
        skip_for_exempt_keys: true
        rules:
          - from: "gpt-5.6-sol-demo"      # 客户端请求的模型
            to: "gpt-5.6-luna"            # 实际转发的模型
            provider: ""                  # 留空则用上面的默认 provider
            keys: []                       # 留空 = 所有 key
      probe_model: ""                # 非空时本地返回策略报告，不调用上游
      probe_header: X-Policy-Probe
      probe_header_value: "1"
      log_decisions: true
      log_format: table
      log_buffer_size: 200
```

### 自动选择供应商（默认）

只需要配置模型转换：

```yaml
model_map:
  provider: ""  # 留空或 auto：自动选择供应商
  rules:
    - from: gpt-6-astra
      to: gpt-6.1-sol
```

规则保留目标模型，由 CPA 的实时模型注册表找到提供该模型的供应商，沿用宿主的凭据调度和失败重试。禁用旧供应商、启用新供应商后无需改规则；被禁用的原模型也可以作为 `from` 匹配。

全局 `provider` 或规则内 `provider` 可填具体 key 固定渠道。规则内省略/空值继承全局设置；规则内填 `auto` 可覆盖全局固定渠道。

自动模式通过插件执行器调用 `host.model.execute` / `host.model.execute_stream`，只把映射后的目标模型交给 CPA。宿主跳过本插件的嵌套路由与拦截，避免递归和重复降级；外层客户端限制、豁免、模型名还原仍生效。

日志中 `provider=auto (CPA)` / `AUTO_PROVIDER_SELECTION` 表示使用宿主调度。它不冒充某个具体渠道；实际供应商可从 CPA 执行日志确认。源模型/目标模型和思考改写均显示在策略日志中。

### 两个协议范围开关

两者相互独立，0.4.4 起刻意解耦：

- **`enforce_all_protocols`**（默认 `true`）——官方客户端准入覆盖**所有协议**。0.4.4 之前，
  `apply_to_non_codex: false` 会让一切非 Responses 请求原样放行，导致
  `/v1/chat/completions` 上任何客户端拿任意合法 key 都能调通，而同一个 key 在
  `/v1/responses` 上会被 403。置 `false` 可退回旧行为。
- **`apply_to_non_codex`**（默认 `false`）——**模型映射和思考降级**是否也作用于
  Claude / Gemini / OpenAI-chat 流量。它不再影响准入。

`/v1/models` 这类非推理接口不经过拦截器，两种设置都不影响它。

### 准入信号

`X-OpenAI-Internal-Codex-Responses-Lite: true`（+2，强）、`Originator` 含 `codex`（+2，强）、
UA 前缀 `codex-tui/` / `codex_cli_rs/` / `codex-cli/`（+2，强）、`X-Codex-Turn-Metadata`（+1）、
`X-Codex-Window-Id` 或 `X-Client-Request-Id`（+1）、`X-Codex-Beta-Features`（+1）、会话头（+1）、
body 形态（+1）。必须命中至少一个**强**信号且总分过阈值，避免单个可伪造的头就放行。

## 审计日志

`log_format: table`（默认）每个请求打印一张对齐的表，`log_format: line` 是单行 `key=value`，便于 grep：

```
[codex-policy] +-- codex-policy --------------------------------------------
[codex-policy] |  phase      : policy
[codex-policy] |  time       : 2026-10-01 23:12:37.751
[codex-policy] |  request_id : 4497d57e-df5
[codex-policy] |  api_key    : sk-W...NiHm   exempt=false
[codex-policy] |  protocol   : openai-response   client=codex-tui/0.159.3
[codex-policy] |  model      : gpt-5.6-sol-demo -> gpt-5.6-luna  via provider=openai-compatible-or
[codex-policy] |  effort     : xhigh -> medium
[codex-policy] |  admission  : ALLOWED  score=7/5  signals=[responses_lite originator:codex-tui ...]
[codex-policy] |  signals    : responses_lite, originator:codex-tui, ...
[codex-policy] |  actions    : MODEL_MAPPED, EFFORT_DOWNGRADED
[codex-policy] |  result     : FORWARDED
[codex-policy] +-- ----------------------------------------------------------------
```

`phase: route` 是模型路由决策，`phase: policy` 是准入与思考等级决策。两个钩子之间不共享
request id，policy 表里的 model 行取自 90 秒 TTL 的路由缓存。

```bash
docker compose logs -f cli-proxy-api | grep 'codex-policy'
```

CPA 自身的路由与执行细节在**日志文件**里，不在 docker logs 的 stdout：

```bash
docker exec cli-proxy-api grep 'Use API key' /CLIProxyAPI/logs/main.log | tail   # 实际用的模型
docker exec cli-proxy-api grep 'model router' /CLIProxyAPI/logs/main.log | tail   # 路由告警
```

## 管理面板菜单

插件注册两个菜单项（CPA 面板 → 插件 → codex-policy）：

| 菜单 | 路径 | 内容 |
|---|---|---|
| 请求日志 | `/v0/resource/plugins/codex-policy/logs` | 每个请求的密钥、客户端、请求模型、实际转发模型、思考强度、准入分数、处理动作 |
| 配置说明 | `/v0/resource/plugins/codex-policy/config` | 每个配置项的类型、默认值、当前生效值、说明 |

两个页面都是自包含 HTML（无外部依赖），纯浏览器访问即可：

```bash
curl -s http://127.0.0.1:8080/v0/resource/plugins/codex-policy/logs   | head -40
curl -s http://127.0.0.1:8080/v0/resource/plugins/codex-policy/config | head -40
```

同样的数据有 JSON 形式（`?format=json`），走 CPA 的管理认证，适合脚本轮询：

```
GET /v0/management/plugins/codex-policy/logs?limit=50&format=json
GET /v0/management/plugins/codex-policy/settings?format=json
```

### 在页面上保存配置

配置页每个字段都能直接改，保存由插件自己写回：

```
POST /v0/management/plugins/codex-policy/settings
Authorization: Bearer <management.secret-key 明文>
Content-Type: application/json

{"official_min_score": 6, "effort.target": "high"}
```

body 是**平铺的点号键**（只提交你实际改动的字段），值为字符串或原生类型。插件按
`config_fields.go` 的声明做类型转换与校验，非法值返回 400 且不落盘。

写入流程：

1. 读 `config.yaml`，只替换 `plugins.configs.codex-policy` 那一段的**文本行范围**，
   其余字节完全不动（保住你在其它段落里写的注释和格式）；
2. 给每个顶层键补上来自 `config_fields.go` 的中文提示注释，让配置自解释；
3. YAML 解析校验通过后备份成 `config.yaml.bak-before-codex-policy`；
4. 原子替换（`config.yaml` 是单文件 bind mount 时宿主返回 `EBUSY`，退化为原地重写）；
5. 用新内容走一遍 `configureRaw()`，插件自身读取的配置**立即生效，无需重启**。

两个限制：

- `enabled` 和 `priority` 由宿主读取，页面标了「需重启」，改完要重启容器；
- 保存接口走宿主管理认证，未带密钥返回 401。

> CPA 自己的 `GET/PUT/PATCH /v0/management/plugins/<id>/config` 是另一套实现：它能写配置并
> 触发热重载，但会**整体重新序列化 `config.yaml`**，全文件注释和缩进都会丢失。插件因此避开
> 那条路径，改用 `/settings`。

### 热加载：实际生效的范围

插件 `/settings` 保存会立即应用插件配置。宿主也能监听配置文件变化；单文件 Docker bind mount 下，编辑器或 `sed -i` 原子替换文件可能让容器继续读取旧 inode。手动修改后核对容器内配置和加载日志；若未生效，重启容器重新挂载配置。

资源路由（`/v0/resource/...`）按 CPA 设计**不做管理认证**，因此日志页只保存脱敏 key
（`sk-W...NiHm`）。仍建议只在可信网络里暴露管理端口。

## curl 验收

```bash
K='<你的 CPA access key>'
H=(-H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
   -H 'X-OpenAI-Internal-Codex-Responses-Lite: true' -H 'Originator: codex-tui' \
   -H 'User-Agent: codex-tui/0.159.3' -H 'session_id: s-verify')

# M1 模型映射（非流式）：请求 sol-demo，上游实际是 luna
curl -s http://127.0.0.1:8080/v1/responses "${H[@]}" \
  -d '{"model":"gpt-5.6-sol-demo","input":"say PONG","reasoning":{"effort":"low"},"stream":false}' \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["model"])'

# M2 模型映射 + 思考降级（流式）：xhigh 应变 medium，model 应保持 sol-demo
curl -sN http://127.0.0.1:8080/v1/responses "${H[@]}" \
  -d '{"model":"gpt-5.6-sol-demo","input":"say PONG","reasoning":{"effort":"xhigh"},"stream":true}' \
  | grep -o '"effort":"[^"]*"\|"model":"[^"]*"' | sort | uniq -c

# M3 非官方客户端：应返回 403
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'User-Agent: python-requests/2.31' \
  -d '{"model":"gpt-5.6-sol-demo","input":"hi","reasoning":{"effort":"xhigh"}}'

# M4 同一个非官方客户端打 chat/completions：也应 403（0.4.4 修复）
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'User-Agent: python-requests/2.31' \
  -d '{"model":"gpt-5.6-sol-demo","messages":[{"role":"user","content":"hi"}]}'
```

判断映射是否真的生效，要看 CPA 执行日志里的**实际模型**，而不是响应里的 `model` 字段：

```bash
docker exec cli-proxy-api grep 'Use API key' /CLIProxyAPI/logs/main.log | tail -3
```

### 用探针做零成本验证

`probe_model` 必须填**可路由的真实模型**（模型解析发生在拦截之前），且请求必须额外带
`X-Policy-Probe: 1`。这个保护是必需的：否则探针会截断真实流式响应，表现为
`stream closed before response.completed`。

```bash
# 探针：本地返回策略报告，不调用上游
curl -s http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $K" \
  -H 'X-OpenAI-Internal-Codex-Responses-Lite: true' -H 'Originator: codex-tui' \
  -H 'User-Agent: codex-tui/0.159.3' -H 'session_id: sess-1' \
  -H 'X-Policy-Probe: 1' \
  -d '{"model":"<REAL_MODEL>","input":"ping","reasoning":{"effort":"xhigh"},"stream":false}'
```

**生产环境务必把 `probe_model` 留空。** 若所填模型同时承载真实流量，没带探针头的真实请求
会被本地 JSON 顶替，表现为 `stream closed before response.completed`。

## 已知边界

- 准入基于请求特征，不是密码学证明；目标是劝退第三方客户端，不是抵御定向伪造。
- `exempt_api_keys` 用常量时间比较，但 key 仍以明文存于 `config.yaml`。
- 目标模型 `to` 始终固定；没有供应商提供该模型时请求会失败，不会自动改成别的模型。
- 自动供应商模式支持推理请求；token count 接口返回 501。启用 CPA Home 模式时，宿主不支持这种插件执行器路由。
- 客户端可见的模型名就是响应里的 `model` 字段，无法阻止客户端通过 `store: true` 或
  `include` 之类的字段观察上游返回的其他信息。

验收记录和可复现命令见 [docs/verification-0.5.0.md](docs/verification-0.5.0.md)。

## 变更日志

### 0.5.0

- 默认自动选择供应商：`model_map.provider` 留空或 `auto`，只保留 `from/to`；显式固定渠道继续支持。
- 交给 CPA 原生模型注册表和凭据调度处理，支持供应商切换、目标模型缺失时报错，避免选择不提供目标模型的渠道。
- 流式桥接包含取消与卸载清理，逐帧改写支持跨 chunk 字节拆分；保留 SSE 的 CRLF 分隔符，并处理不带末尾换行的原生 Codex 事件。
- Chat 协议的思考降级改写原有 `reasoning_effort` 字段。
- 自动模式推理支持 HTTP JSON / SSE；token count 返回 501，CPA Home 模式不支持插件执行器路由。
- 增加单元测试及 `tests/integration_auto.py`，使用隔离 CPA 和本地模拟上游验收。

### 0.4.5

- 路由前核对宿主提供的可用 provider 列表。目标不可用时记录 `PROVIDER_UNAVAILABLE` / `ROUTE_REJECTED`，列出配置目标与可用提供商，不再缓存为已应用映射。
- 路由钩子日志改为 `ROUTE_PROPOSED`：表示提交映射建议；进入后续 policy 阶段才说明宿主已接受路由。
- 更换 AI 提供商后要同步更新 `model_map.provider`。例如停用 `or`、启用 `subapi` 后，目标应改为 `openai-compatible-subapi`。规则中的 `provider` 若非空，也要一并核对。

### 0.4.4

- **修复：非 Responses 协议绕过准入。** 此前 `apply_to_non_codex: false`（默认值）会让一切
  非 codex/responses 的 `source_format` 直接放行，导致任何客户端拿任意合法 access key 都能调
  `/v1/chat/completions`，而同一个 key 在 `/v1/responses` 上会被 403。现通过新增的
  `enforce_all_protocols`（默认 `true`）把准入与改写范围解耦。
- 新增回归测试 `TestAdmissionCoversNonCodexProtocols`、
  `TestRewriteAppliesToNonCodexOnlyWhenEnabled`。

### 0.4.3 及更早

- 修复审计环形缓冲区数据竞争、SSE 改写 panic 与文本含 `"data: "` 时的 JSON 损坏、
  非法 YAML 被静默逗号切分、`arrayAsYAML` 未加引号导致 `#` / `:` 截断、路由缓存按脱敏 key
  串号、`splicePluginBlock` 遇空行截断产生重复键等问题。

## 许可

随附使用，面向 CLIProxyAPI 部署场景。
