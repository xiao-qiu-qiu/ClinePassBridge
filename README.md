# ClinePassBridge

把 Cline Pass 接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)，通过 CPA 的 OpenAI 兼容接口调用模型。

## 功能

- 管理多个 Cline Pass API key，设置账号优先级，查看套餐用量和额度估计。
- 自定义模型名称，选择上游 provider，按模型设置思考强度映射。
- 支持 SSE 流式输出，也能将上游流式响应合并为普通 JSON。
- 查看请求日志、实际使用的账号和 provider、首字耗时、输出速率及缓存命中率。
- 在配置概览或「钉上游」页测试模型，选择测试凭据与思考强度；遇到上游团队限流时短暂退避，减少重复请求。

## 安装与配置

需要支持动态库插件、且与本插件接口兼容的 CPA。先在 CPA 配置中启用插件：

```yaml
plugins:
  enabled: true
  dir: plugins
```

1. 在 CPA 插件市场的 **CLIProxyAPI源（官方源）** 中安装 **ClinePassBridge**。
2. 打开插件管理页：`/v0/resource/plugins/clinepassbridge/console`。
3. 点击「添加凭据」，填入 Cline Pass API key。
4. 点击「添加模型 → 获取上游模型」，勾选要使用的模型，也可以手动添加映射。

管理页会尝试沿用 CPA / CPAMP 记住的登录信息。若提示输入密钥，直连 CPA 时填 CPA 管理密钥，经 CPAMP 完整模式访问时填该面板的管理密钥。勾选「在此浏览器记住管理密钥」后，下次打开可继续使用。

首次安装的模型列表为空，需要先添加模型。导入时默认去掉客户端名称的 `cline-pass/` 前缀，上游 ID 保留前缀；模型是否可用取决于账号的套餐权限。

## 调用模型

客户端使用 CPA 的 API 地址和客户端 API key。请求中的 `model` 要与管理页的「客户端模型名称」一致。

例如，添加以下映射：

| 客户端模型名称 | 上游模型 ID |
| --- | --- |
| `deepseek-flash` | `cline-pass/deepseek-v4.1-flash` |

然后向 CPA 的 `/v1/chat/completions` 发送：

```json
{
  "model": "deepseek-flash",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

几个常用设置：

- **非流式模式**：默认 `stream-aggregate`，客户端收到普通 JSON，上游仍使用流式请求。
- **请求超时**：新装默认 600 秒。升级会保留原设置，长任务可在「请求与日志设置」中调高。
- **思考强度映射**：默认关闭，按模型单独配置。例如将 `medium` 映射为 `high`，日志会显示 `medium → high`。
- **账号优先级**：数值越大越优先，实际调度由 CPA 决定。调整后若希望已有对话重新选账号，可在「配置概览」点击「重新分配会话」。

模型测试会消耗所选账号的少量额度。它直接调用 Cline；要确认 CPA 全局规则是否生效，需要通过客户端发起正式请求。

插件独立打开且没有保存主题设置时默认使用纯白；嵌入 CPA 时跟随宿主主题，已保存的主题设置继续生效。

## 文档

- [进阶配置](docs/configuration.md)：登录与备份、上游选择、思考强度映射、日志和限流。
- [套餐用量](docs/plan-usage.md)：用量查询、额度估计的计算方式和误差。

容器部署时，请持久化插件目录、CPA 的 `auth-dir` 和插件数据目录（默认 `plugins/clinepassbridge-data`）。API key 保存在 `auth-dir`，配置、日志和统计保存在插件数据目录。

## 从源码构建

在仓库根目录执行以下命令，需要 Docker，产物为 `dist/clinepassbridge.so`（Linux amd64）：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

项目使用 Go 1.26。[发布工作流](.github/workflows/release.yml) 构建 Linux、macOS 的 amd64 / arm64 和 Windows amd64，并生成 ZIP 包与 `checksums.txt`。

## 许可

[MIT License](LICENSE)。独立实现，曾参考 [`cline-pass-switcher`](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 的公开协议行为，未复制其代码。
