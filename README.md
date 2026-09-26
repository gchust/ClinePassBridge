# ClinePassBridge

> 本仓库的 fork 维护分支为 `custom/main`，`main` 用于镜像上游。Fork 默认
> 仅发布 macOS amd64/arm64，版本格式为 `vX.Y.Z-fork.N`。安装本 fork 请使用
> Release 附带的 `registry.json`；下文的官方插件市场入口仍对应上游。
> 分支、同步与发布规则见 [Fork 维护说明](docs/fork-maintenance.md)。

ClinePassBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Cline Pass 插件。它把 Cline Pass API key 接入 CPA 的凭据系统，提供模型别名映射、Chat Completions 协议适配、真实 SSE 流及请求观测等功能。

## 功能

- 在插件管理页导入 Cline Pass API key，凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存 key。
- 按用户配置的映射列表，将客户端模型名映射为指定上游 ID；初始列表为空。
- “添加模型”弹窗内可获取上游模型；获取后默认使用不带 `cline-pass/` 的客户端名称，映射到带 `cline-pass/` 前缀的服务端模型。
- 非流式支持 `native`（解包 Cline 原生 `success/data`）、`native-fallback`（原生遇到空内容错误时尝试流式聚合）和 `stream-aggregate`（直接由 SSE 聚合）三种模式。默认 `stream-aggregate`，直接聚合上游 SSE 后返回 JSON，跳过原生非流式尝试。规避 Cline 原生非流式空响应导致的 500 错误。
- 流式请求转发为真正的 SSE，处理跨网络分块的事件、用量与终止信号；从上游响应元数据记录实际 provider。
- 管理页展示凭据、模型映射、请求状态、耗时、用量、实际 provider 和尝试记录。
- 会话粘滞（默认开启）：每个（上游模型，凭据）复用一个 `X-Task-Id`，由 Cline 网关把渠道钉在该模型计划首位，例如 `deepseek-v4.1-flash` 始终走 DeepSeek 官方。见下文“钉上游：会话粘滞”。
- 日志保留条数可设为 50–99999999；请求日志以 JSON Lines 追加写入，不再每次请求重写全部历史。
- 每条映射支持“测试模型”，可选择测试凭据，发送简短消息验证是否能完整返回响应。完整响应耗时按绿（小于 3 秒）、黄（3–10 秒）、红（10 秒及以上）显示；失败或超时标红，点击可展开完整错误详情，敏感密钥会被遮蔽。

模型测试固定使用流式聚合，沿用请求超时设置，单次最多输出 64 tokens，不自动重试或切换账号；会消耗所选账号的少量额度，并计入请求日志与用量统计。测试结果仅代表该账号在测试时的可用性。错误详情受配置中的最大响应大小限制，超过上限时明确标记截断。凭据独立代理暂不支持此管理页测试，普通全局代理可用。


## 安装

ClinePassBridge 已收录到 CPA 内置官方插件市场，启用插件后直接搜索安装即可，无需添加额外市场源。需要 CLIProxyAPI v7.3.12 或兼容的插件 ABI。以下是通用配置片段，按现有配置合并：

```yaml
plugins:
  enabled: true
  dir: plugins
```

在 CPA 的插件市场找到来源为 **CLIProxyAPI源（官方源）** 的 **ClinePassBridge** 并安装。市场从本仓库 Release 下载与宿主平台匹配的 ZIP 和 `checksums.txt`，核验 ZIP 的 SHA-256。各 ZIP 根目录分别是 `clinepassbridge.so`（Linux）、`clinepassbridge.dylib`（macOS）或 `clinepassbridge.dll`（Windows）；安装后的文件名带版本，但插件 ID 始终是 `clinepassbridge`。

市场安装会写入插件配置。插件加载后打开：

```text
/v0/resource/plugins/clinepassbridge/console
```

页面优先复用同源 CPA 管理中心通过“记住密码”保存的登录信息，并核对其服务器地址。未保存登录信息、存储不可用或管理中心跨域时，才提示输入 **CPA 管理密钥**；手动输入的密钥仅保存在当前页面内存。插件不会额外持久化管理密钥。CPA 的管理 API 必须已启用；接口鉴权仍由 CPA 控制。进入页面后点“添加凭据”，导入自己的 Cline Pass API key，再检查或选择模型映射。

插件配置（`settings.json`）、请求记录（`requests.jsonl`，旧版 `requests.json` 会在首次写入时自动迁移）与粘滞会话（`sticky.json`）默认写在 `plugins/clinepassbridge-data`；凭据文件写在 CPA 配置的 `auth-dir`。使用容器时，应分别持久化这两个目录及插件目录。备份时也应覆盖这两处数据。

## 模型与路由

客户端调用 CPA 的 OpenAI 兼容接口时，`model` 必须与管理页映射列表中的“客户端模型名称”完全一致。插件仅注册并接受已配置的名称，不自动添加别名，也不会把“上游模型 ID”隐式当成另一个客户端名称；如需直接使用上游 ID 调用，请单独添加同名映射。

初次安装的映射列表为空。可以手动添加，或点击“添加模型 → 获取上游模型”，勾选后确认添加。还需确认 Cline Pass 账号有对应模型的使用资格，模型目录出现某个 ID 不代表订阅可调用。

例如，先添加客户端名称 `deepseek-flash`、上游 ID `cline-pass/deepseek-v4.1-flash` 的映射，然后才能通过本插件发起以下请求：

```json
{
  "model": "deepseek-flash",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

升级会保留已保存的映射列表。旧版默认映射若已写入配置，也会作为已有配置保留；不需要的条目可在管理页删除，删除或清空后不会自动补回。

日志中的实际 provider 来自上游响应；未回报时显示“未知”。

### 钉上游：会话粘滞

Cline 网关会静默忽略请求体里的钉渠道字段（`provider.only / order / ignore`、`providerOptions.gateway.*`），本插件因此直接拒绝这类请求，避免“以为钉住了”。网关实际认可的只有按 `X-Task-Id`（响应里回显为 `routing.clientSessionId`）做的服务端亲和：

- 新 task ID 的第 1 次请求落在该模型执行计划的首位，`routing.affinity.outcome` 为 `no_pin`；
- 同一 task ID 的第 2 次请求起，网关回报 `confirmed` 并在服务端记住渠道，与请求体无关。
- 钉住按（会话，模型）分别记录：同一会话换一个模型，会重新从 `no_pin` 开始。
- 不带 `X-Task-Id` 时，网关按 API key 派生一个默认会话（`sess-…`，与 prompt、User-Agent 无关），同样会钉住，但客户端无法观测，也无法重置。

开启会话粘滞后，插件为每个（上游模型，凭据）保存一个 task ID 并持续复用，重启后仍沿用（网关记忆仍在）。每次响应后按网关元数据判断：

- 已 `confirmed` 且钉住的渠道就是目标：保持会话，即使本次因限流临时回退到别的渠道；
- 首跳实际落在别的渠道（首位失败回退），或网关钉在了别的渠道：立即换新 task ID，下一次请求重新落到计划首位，避免被钉在回退渠道上。

**新会话预热**（`sticky_warmup_attempts`，默认 10，0 关闭）：流式响应里实际渠道只在最后一个数据块回报，发现落错时正文已经发给客户端。因此一个 task ID 在第一次真实使用前，插件先用它发一条极小请求（`Reply with OK only.`，`max_tokens` 32）：

- 实际渠道就是目标：真实请求沿用这个 task ID，网关从这一条起 `confirmed`，真实请求照常流式输出；
- 落在别的渠道（例如 deepseek 首跳失败回退到 alibaba）：换新 task ID，间隔 1、2、3… 秒（最多 5 秒）重试，直到达到次数上限；
- 5xx 或超时说明这个 task ID 没被服务过，原样重试；4xx（额度、鉴权）立即停止；
- 单次预热最多 20 秒，全部预热最多占一半请求超时；次数用完后真实请求在新的 task ID 上照常发出；
- 同一会话的并发请求共用一次预热；预热记录在该请求日志的“尝试明细”里，标记为“预热”。

模型映射的“测试粘滞”会重置该模型在所选凭据下的会话，完整跑一遍预热与一次确认请求，逐条列出 task ID、实际渠道和粘滞状态，并计入请求日志。

目标默认是该模型的计划首位（从 `routing.planningReasoning` 的执行顺序解析）。只能钉“计划首位”这一个渠道：例如 `deepseek-v4.1-flash` → `deepseek`、`glm-5.3` → `baseten`，顺序由网关决定，指定不了其他渠道。模型映射可填“期望上游”（`sticky_provider`）；若它不是计划首位，管理页如实标记“无法钉住期望上游”，不会假装钉住，也不会反复换会话。

管理页“会话粘滞”面板列出每个会话的状态、计划首位与渠道数、最近实际上游和换会话次数，可单独或全部重置。关闭粘滞后请求不带 `X-Task-Id`，由网关自动路由。

非流式三种模式用于应对 Cline 返回格式和偶发空内容，推荐的 `stream-aggregate` 从第一次请求就使用流式上游，客户端仍收到非流式 JSON；它不会消除上游本身的错误。可选的 `native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Cline 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配；未携带此元数据的内部 Claude 调用尚未覆盖。

## 从源码构建

项目使用 Go 1.26、标准 C ABI 和 `gopkg.in/yaml.v3`。在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

构建阶段使用 `golang:1.26-bookworm`，并以 `CGO_ENABLED=1`、`-buildmode=c-shared` 编译 `./cmd/passbridge`。输出 `dist/clinepassbridge.so`，目标为 Debian 12 兼容的 Linux amd64 动态库。本地 Windows 无需安装 C 编译器，构建交给 Docker。

上游的[构建工作流](.github/workflows/release.yml) 覆盖 Linux、macOS 和 Windows；它在本 fork 中禁用。本 fork 的 [macOS 检查](.github/workflows/macos-ci.yml) 使用 `macos-15-intel`/`macos-15` 原生测试并构建 amd64/arm64；PR 合并到 `custom/main` 后，[发布工作流](.github/workflows/release-on-custom-merge.yml) 生成两个 ZIP、`checksums.txt` 和 fork 专用 `registry.json`。资产命名与 ZIP 布局沿用 [CPA 官方插件市场规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)。

市场 registry 使用 CPA v7.3.12 的 `schema_version: 1`、`github-release` 安装类型。它是 CPA 商店入口，不是一个可直接安装的 `.so` URL；若使用自己的市场源，也必须托管符合该 schema 的 JSON registry，并提供对应 GitHub Release 资产。

## 许可与来源

本项目按 [MIT License](LICENSE) 发布。ClinePassBridge 为独立实现；[`cline-pass-switcher`](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 只作为公开协议行为的研究参考，没有复制其代码。Cline Pass 与 CLIProxyAPI 分别由各自项目维护。
