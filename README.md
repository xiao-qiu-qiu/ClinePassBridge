# ClinePassBridge

ClinePassBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Cline Pass 插件。它把 Cline Pass API key 接入 CPA 的凭据系统，提供模型别名映射、Chat Completions 协议适配、真实 SSE 流及请求观测等功能。

## 功能

- 在插件管理页导入 Cline Pass API key，凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存 key。
- 按用户配置的映射列表，将客户端模型名映射为指定上游 ID；初始列表为空。
- “添加模型”弹窗内可获取上游模型；获取后默认使用不带 `cline-pass/` 的客户端名称，映射到带 `cline-pass/` 前缀的服务端模型。
- 非流式支持 `native`（解包 Cline 原生 `success/data`）、`native-fallback`（原生遇到空内容错误时尝试流式聚合）和 `stream-aggregate`（直接由 SSE 聚合）三种模式。默认 `stream-aggregate`，直接聚合上游 SSE 后返回 JSON，跳过原生非流式尝试。规避 Cline 原生非流式空响应导致的 500 错误。
- 流式请求转发为真正的 SSE，处理跨网络分块的事件、用量与终止信号；从上游响应元数据记录实际 provider。
- 管理页展示凭据、模型映射、请求状态、耗时、用量、实际 provider 和尝试记录。
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

日志中的实际 provider 来自上游响应；未回报时显示“未知”。

非流式三种模式用于应对 Cline 返回格式和偶发空内容，推荐的 `stream-aggregate` 从第一次请求就使用流式上游，客户端仍收到非流式 JSON；它不会消除上游本身的错误。可选的 `native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Cline 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配；未携带此元数据的内部 Claude 调用尚未覆盖。

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
