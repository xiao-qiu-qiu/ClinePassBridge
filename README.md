<div align="center">

# ClinePassBridge

把 Cline Pass 接到 CPA，在你常用的 OpenAI 兼容客户端里调用。

[![Release](https://img.shields.io/github/v/release/xiao-qiu-qiu/ClinePassBridge?style=flat&color=2563eb)](https://github.com/xiao-qiu-qiu/ClinePassBridge/releases/latest)
[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20Windows-64748b)](https://github.com/xiao-qiu-qiu/ClinePassBridge/releases)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/github/license/xiao-qiu-qiu/ClinePassBridge?color=16a34a)](LICENSE)

[下载安装](https://github.com/xiao-qiu-qiu/ClinePassBridge/releases) · [使用文档](docs/configuration.md) · [反馈问题](https://github.com/xiao-qiu-qiu/ClinePassBridge/issues)

</div>

本项目是 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的插件，带一个网页管理界面。在界面内添加 Cline Pass API key 和模型后，就可以通过 CPA 发起请求。

## 本项目能做什么

- **管理多个账号**：查看套餐用量、调整调度优先级、开关账号调度。额度用完自动暂停，额度恢复自动恢复。
- **映射模型**：自定义模型名映射，避免客户端手敲 `cline-pass/` 前缀。还能映射思考强度。
- **钉上游**：按模型选择预设或自定义 `providerOptions` JSON；预设支持提供方允许列表（`only`）和[能力过滤](docs/upstream-capability-filter.md)（`has`），DeepSeek 系列在列表中置顶。默认 1017 映射会附加官方路由区域规则。
- **查看请求情况**：记录日志，显示上游、首字耗时、输出速度和缓存命中率。
- **调整请求行为**：统一设置错误规则、提前截断流式卡住的请求。还提供实验性代理池，按需开启。

## 安装与配置

需要支持动态库插件和 JSON ABI v1 的原版 CPA。已核对 v8.0.13、v8.0.29 的相关接口与错误处理路径，具体范围见 [CPA 兼容性](docs/cpa-compatibility.md)。先在 CPA 配置里启用插件：

```yaml
plugins:
  enabled: true
  dir: plugins
```

1. 在 CPA 插件市场的 **CLIProxyAPI源（官方源）** 中安装 **ClinePassBridge**。
2. 打开管理页 `/v0/resource/plugins/clinepassbridge/console`，在「凭据管理」中添加 Cline Pass API key。
3. 进入「模型映射」，点击「添加模型 → 获取上游模型」，勾选要用的模型。
4. 在客户端填入 **CPA 的 API 地址和 CPA 的客户端 API key**，选择刚添加的模型即可。

首次安装只默认添加 `deepseek-v4.1-flash-1017` → `cline-pass/deepseek-v4.1-flash-1017` 映射，使用预设模式和「插件配置优先」，提供方为 `deepseek`，能力为 `vision,implicit-caching`，并自动附加[官方路由区域规则](docs/configuration.md#deepseek-1017-默认-provider-策略)。旧的 `only + has` 组合已不能可靠排除私有通道。

在「钉上游」每个模型行的「编辑参数」中可切换预设与自定义模式。自定义 JSON 填 `providerOptions` 内部的对象，不包外层 `providerOptions`；该模式不自动补入提供方、能力或区域规则。看到「已保存」后，用「测试模型」确认正常生成且「实际上游」为 `deepseek`。

升级时，已有 1017 映射的 `has` 缺失或为 `null` 会补入上述默认能力，已有提供方列表和自定义能力保持原值；显式保存 `has: []` 可退出能力过滤，其他上游模型不补能力默认值。实际命中的提供方仍以日志为准。其他模型按第 3 步添加，导入时客户端模型名默认去掉 `cline-pass/` 前缀；旧映射不会自动换成 1017，需自行添加。

管理页会尝试复用 CPA / CPAMP 的登录信息。如果提示输入密钥，直连 CPA 时填 CPA 管理密钥，经 CPAMP 完整模式访问时填面板管理密钥。

也可以从 [Releases](https://github.com/xiao-qiu-qiu/ClinePassBridge/releases) 下载 ZIP 手动安装。按 **运行 CPA 的系统和架构** 选择：Linux / macOS 支持 amd64、arm64，Windows 支持 amd64。每次发布附带 `checksums.txt`。

## 调用模型

假设你添加了这条模型映射：

| 客户端模型名称 | 上游模型 ID |
| --- | --- |
| `deepseek-flash` | `cline-pass/deepseek-v4.1-flash` |

向 CPA 的 `/v1/chat/completions` 发送请求，`model` 填客户端模型名称：

```json
{
  "model": "deepseek-flash",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

## 用之前看一眼

- **流式卡住时提前截流默认关闭。** 开启后，从首段有效输出后计时，默认连续 20 秒没有新的有效输出才触发；向下游发送的耗时不计入空闲时间，不缩短首字等待。开关和时长在「配置概览」里调整，已保存的 `true` / `false` 和自定义秒数升级后继续保留。
- **Debug 默认关闭。** 按需开启额外流诊断；普通请求日志、错误和用量照常记录，详见 [进阶配置](docs/configuration.md#流式卡住时提前截流)。
- **升级前保留数据。** 容器部署请持久化插件目录、CPA 的 `auth-dir` 和插件数据目录（默认 `plugins/clinepassbridge-data`），升级前备份。

更多设置和细节放在下面，遇到需要时再查：

| 文档 | 内容 |
| --- | --- |
| [进阶配置](docs/configuration.md) | 登录、备份、上游选择、思考强度、错误规则和日志 |
| [套餐用量](docs/plan-usage.md) | 用量查询、额度估计和误差 |
| [调度与代理出口](docs/scheduling.md) | 账号调度、额度保护、代理池和限流 |
| [CPA 兼容性](docs/cpa-compatibility.md) | 支持的接口、流式错误、重试和冷却行为 |

## 从源码构建

项目使用 Go 1.26。装好 Docker 后，在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

产物是 `dist/clinepassbridge.so`（Linux amd64）。其他平台的构建方式见 [发布工作流](.github/workflows/release.yml)。

## 许可与致谢

[MIT License](LICENSE)。项目独立实现，曾参考 [cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 的公开协议行为，未复制其代码。

## 友情链接

[LINUX DO](linux.do)
