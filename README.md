# ClinePassBridge

ClinePassBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Cline Pass 插件。它把 Cline Pass API key 接入 CPA 的凭据系统，提供模型别名映射、Chat Completions 协议适配、真实 SSE 流及请求观测等功能。

## 功能

- 在插件管理页导入 Cline Pass API key，凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存 key。每条凭据可设置**优先级**（1–100，留空表示不设置）：数值越大越优先被 CPA 调度。插件只把该值写进凭据文件，具体调度与是否生效由 CPA 决定；会话黏性沿用 CPA 自带的 `routing.session-affinity`，插件不另建机制。
- 按用户配置的映射列表，将客户端模型名映射为指定上游 ID；初始列表为空。
- “添加模型”弹窗内可获取上游模型；获取后默认使用不带 `cline-pass/` 的客户端名称，映射到带 `cline-pass/` 前缀的服务端模型。
- 非流式支持 `native`（解包 Cline 原生 `success/data`）、`native-fallback`（原生遇到空内容错误时尝试流式聚合）和 `stream-aggregate`（直接由 SSE 聚合）三种模式。默认 `stream-aggregate`，直接聚合上游 SSE 后返回 JSON，跳过原生非流式尝试。规避 Cline 原生非流式空响应导致的 500 错误。
- 流式请求转发为真正的 SSE，处理跨网络分块的事件、用量与终止信号；从上游响应元数据记录实际 provider。
- 管理页展示凭据、模型映射、请求状态、耗时、用量、实际 provider、**实际使用的凭据**和尝试记录。被本地退避拦下的请求没有上游 provider，列表与详情统一显示为「本地退避」，并可用该值筛选。
- 每条映射支持“测试模型”，可选择测试凭据，发送简短消息验证是否能完整返回响应。完整响应耗时按绿（小于 3 秒）、黄（3–10 秒）、红（10 秒及以上）显示；失败或超时标红，点击可展开完整错误详情，敏感密钥会被遮蔽。
- 每条模型映射可选填「上游 provider 允许列表」：留空保持自动路由；只填一个即强制固定到该上游；填多个则限定上游只能在这些托管方之间选择。Cline 侧的同一个模型通常由多家托管方共同承载，固定列表用于避开被限流的一家。「钉上游」页把全部模型的允许列表和上游 provider 策略集中在一处编辑，无需逐个打开模型弹窗。首次使用（模型从未设置过该字段）时，DeepSeek 系列会自动填入单 `deepseek`；其他模型保持自动路由。可固定的上游不是固定清单，而是按模型**探测**得到：点「探测可钉上游」向该模型发一个极小请求，从上游回报的路由信息里读出候选（也可用「探测全部模型」逐个探测）。结果依赖所用凭据——被上游转到私有通道的账号不会回报候选，因此该页提供凭据选择器，切换后需重新探测。把列表显式清空表示不再固定，不会被重新填回。该页顶部有当前生效状况的提示：上游目前只对部分模型应用白名单。

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

页面复用同源 CPA 管理中心通过“记住密码”保存的登录信息，并核对其服务器地址，兼容根地址及 `/v0/management`、`/v8/management` 后缀。未保存登录信息、存储不可用或管理中心跨域时，可输入 **CPA 管理密钥**；手动密钥在请求验证成功后保存到当前标签页的 `sessionStorage`，按 CPA 地址隔离，刷新或切回插件无需重填。清除密钥、鉴权失败或结束标签页会话后移除；浏览器禁止存储时仅保留在当前页面内存。插件不向 `localStorage` 额外保存密钥。CPA 的管理 API 必须已启用；接口鉴权仍由 CPA 控制。进入页面后点“添加凭据”，导入自己的 Cline Pass API key，再检查或选择模型映射。

插件配置与请求记录默认写在 `plugins/clinepassbridge-data`；凭据文件写在 CPA 配置的 `auth-dir`。使用容器时，应分别持久化这两个目录及插件目录。备份时也应覆盖这两处数据。

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

日志中的实际 provider 来自上游响应；未回报时显示“未知”。请求失败时也会从上游错误正文里取出本次实际命中的 provider，并记录它来自哪个字段，因此限流记录同样能看出是哪一家托管方。

Cline 会把请求里的 `providerOptions.gateway` 原样转交给上层的 Vercel AI Gateway：`only` 是白名单，`order` 只是优先级提示，网关仍可能在同一白名单内自行改选。因此只列一个托管方时请求必定固定（该家满容量时整条请求会失败），列多个时则由网关在白名单内回退。日志展示的 provider 正是网关实际使用的那一家。

「上游 provider 策略」在「钉上游」页，决定调用方自己传的 `providerOptions.gateway`（以及等价的顶层 `provider` 简写）如何处理。默认「插件配置优先」：忽略调用方传来的 provider 字段，只有插件里配置的列表生效；未配置列表的模型仍走自动路由。切为「客户端优先」后，调用方传入的字段优先生效，未传的字段由插件配置补齐。两种策略下，被限流而本地退避的请求都会标注是等待哪一家托管方。

「凭据与套餐用量」现支持按账号估计 5 小时、每周、每月的总额度与剩余额度：5 小时/每周结合用量比例变化和请求 token，月总额度按周总额度的两倍估算，按 [Cline 官网参考价格](https://docs.cline.bot/getting-started/clinepass#reference-pricing)折算 USD；关闭页面仍继续采样，同账号多 Key 共享估算，不同账号独立。额度增长 1 个百分点先显示带黄色提示的初步估值，累计 2 个百分点后校准。额度周期重置后继续显示上次有效估值，新周期采够样本后自动更新。失败或缺失用量时保留已知消费，结果以单值展示，误差原因放在悬停提示中。采样条件、缓存计费和估算误差见 [套餐用量说明](docs/plan-usage.md#额度估计)。

请求总时长上限（含思考和输出）新装默认 **600 秒**，避免旧默认 180 秒过早截断长任务（[Issue #1](https://github.com/xiao-qiu-qiu/ClinePassBridge/issues/1)）。升级保留已保存的超时设置；原来仍为 180 秒的用户可在「请求与日志设置」手动调至 600 秒或更高，允许范围 10–1800 秒。

非流式三种模式用于应对 Cline 返回格式和偶发空内容，推荐的 `stream-aggregate` 从第一次请求就使用流式上游，客户端仍收到非流式 JSON；它不会消除上游本身的错误。可选的 `native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Cline 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配；未携带此元数据的内部 Claude 调用尚未覆盖。

上游正常关闭连接、所有输出均有非空 `finish_reason` 且工具调用参数完整时，插件兼容缺少 `[DONE]` 的流；Responses 接口由 CPA 完成协议转换与结束事件。半截 SSE、未完成的输出及传输错误仍报错，已输出的请求不自动重试。请求日志的 `stream_end` 可区分正常标记 `done`、完成后的 EOF `eof_after_finish` 和不完整 EOF；缺少结束信号的错误还会记录输出是否开始、choice 是否全部结束，便于与 Cline 限流或真实中断区分。

## 上游团队限流

上游团队/区域的输入 token 每分钟额度触顶时，插件将 HTTP200 中的 SSE 限流错误识别为 **429 / `upstream_team_rate_limited`**，错误文字带 `[clinepassbridge:team_tpm_limit]`，管理页显示“上游团队限流”。普通请求频率限制保留 `upstream_rate_limited`，套餐额度耗尽单独记录 `upstream_quota_exhausted`；这三类不混为账号失效。

流式调用会在首次实际输出前暂存少量角色/元数据帧，遇到团队限流时通过结构化错误返回宿主。仅该专用标识匹配 CPA 的请求级 `stop` 规则，跳过本次错误造成的凭据冷却并停止宿主继续尝试。普通429、认证失败及套餐耗尽仍沿用宿主原有策略；已有冷却也不会被这条规则清除。此链路的集成验证基准为 CPA v8.0.4。

插件按凭据和真实上游模型维护内存中的短期退避，同模型多个别名共用窗口。优先读取 `Retry-After` 或已知错误中的 `Retry after Ns`，缺失时等待60秒；窗口内直接返回明确的团队限流，不再访问上游，也不新增未知消费。到期仅放行一个恢复探测；探测在输出前取消或失败时，继续保留短暂退避。普通429不会开启这套团队限流窗口。插件重载后窗口重建，后续请求将重新探测上游。

请求日志通过 `error_kind`、`upstream_http_status`、`upstream_error_status`、`retry_at`、`rate_limit_scope` 和 `upstream_skipped` 分别记录错误类别、外层/内层状态、重试时间及本地拦截。第一版不自动重放模型请求；开始输出后的错误仍通过 CPA 现有流错误接口传递，完整状态传播需要宿主支持结构化流错误。

## 从源码构建

项目使用 Go 1.26、标准 C ABI 和 `gopkg.in/yaml.v3`。在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

构建阶段使用 `golang:1.26-bookworm`，并以 `CGO_ENABLED=1`、`-buildmode=c-shared` 编译 `./cmd/passbridge`。输出 `dist/clinepassbridge.so`，目标为 Debian 12 兼容的 Linux amd64 动态库。本地 Windows 无需安装 C 编译器，构建交给 Docker。

[构建工作流](.github/workflows/release.yml) 在普通 push 和 PR 中分别测试并构建五个平台：Linux amd64 使用 Debian 12 Docker 构建，Linux arm64 在 `ubuntu-24.04-arm` 上使用同一 Go 镜像；macOS amd64/arm64 使用原生 `macos-15-intel`/`macos-15`；Windows amd64 使用 `windows-latest` 和 MSYS2 UCRT64 GCC。只有推送与源码版本一致的 `v<version>` 标签且五个作业全部通过时，工作流才汇总发布五个 ZIP 与统一的 `checksums.txt`。资产命名遵循 [CPA 官方插件市场规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)。

市场 registry 使用 CPA v7.3.12 的 `schema_version: 1`、`github-release` 安装类型。它是 CPA 商店入口，不是一个可直接安装的 `.so` URL；若使用自己的市场源，也必须托管符合该 schema 的 JSON registry，并提供对应 GitHub Release 资产。

## 许可与来源

本项目按 [MIT License](LICENSE) 发布。ClinePassBridge 为独立实现；[`cline-pass-switcher`](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 只作为公开协议行为的研究参考，没有复制其代码。Cline Pass 与 CLIProxyAPI 分别由各自项目维护。
