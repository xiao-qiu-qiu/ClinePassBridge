# 上游能力过滤（has）

`has` 按能力标签筛选候选，本身不指定提供方。当前 `cline-pass/deepseek-v4.1-flash-1017` 默认官方路由使用：

- `only: ["deepseek"]`：提供方允许列表，限定公开候选路由中的 provider。
- `has: ["vision", "implicit-caching"]`：要求候选满足这些能力标签。
- `inferenceRegion`：全局范围，并将 `openai-compatible-private` 限为 `zone/us`，用于筛除私有通道。

旧的 `only + has` 组合已不能可靠固定官方。2026-11-04 同凭据对照中，旧组合仍命中 private，加入区域规则后连续两次由 `deepseek` 正常回复 `OK`。下面的 2026-10-23 表格保留原始结果，其组合足以固定官方的结论已失效。`us` 是这里的筛选参数，不代表实际推理位置。

## 在插件中设置

上游 ID 精确为 `cline-pass/deepseek-v4.1-flash-1017` 时，模型配置中的 `has` 缺失或为 `null` 会使用默认能力 `["vision", "implicit-caching"]`。首次安装的默认映射也会带上这两项能力，提供方默认填入 `deepseek`。

升级后，已有 1017 映射如果此前未配置能力，后续请求会开始带上这两项能力条件；已有提供方列表不因此改写。显式保存 `has: []` 可关闭默认能力，自定义的非空列表也原样保留。其他上游模型不补能力默认值。这是可覆盖的模型默认值，最终提供方仍以实际请求结果为准。

在「钉上游」模型行的「编辑参数」中可编辑预设，或切换到自定义 JSON。预设模式下，只有「插件配置优先」、精确 1017 上游 ID、允许列表仅为 `["deepseek"]` 时才自动附加区域规则；清空能力条件不会关闭区域规则。切换客户端优先、清空或更改允许列表、改用其他上游 ID，都退出自动区域规则。

自定义模式直接使用 `provider_options` 对象，不自动补 `only`、`has` 或区域规则。JSON 不包外层 `providerOptions`；切回预设会保留该 JSON。配置优先完全采用配置 JSON，客户端优先则以配置为默认递归合并客户端覆盖，`null` 和 `[]` 也能覆盖，其他命名空间保留合并。字段说明见[模型级自定义参数](configuration.md#模型级自定义参数)。

「探测可钉上游」按已保存的能力条件发起小请求，同时移除提供方允许列表、顺序等其他路由选项，读取符合这些能力条件的候选。候选为空只表示上游没有列出候选；最终保存的组合是否命中 DeepSeek，请通过「测试模型」或正式请求的「实际上游」确认。

## 请求形态

`has` 和区域规则都写在请求体的 `providerOptions.gateway` 中。下面展示当前 1017 预设的路由字段：

```json
{
  "model": "cline-pass/deepseek-v4.1-flash-1017",
  "messages": [{ "role": "user", "content": "只回复 OK" }],
  "stream": true,
  "provider": {"only": ["deepseek"], "order": ["deepseek"]},
  "providerOptions": {
    "gateway": {
      "only": ["deepseek"],
      "order": ["deepseek"],
      "has": ["vision", "implicit-caching"],
      "inferenceRegion": {
        "scope": "global",
        "providers": {
          "openai-compatible-private": {"scope": "zone", "geoRegion": "us"}
        }
      }
    }
  }
}
```

| 能力标签 | 含义 |
| --- | --- |
| `vision` | 网关标记的图片输入能力 |
| `implicit-caching` | 网关标记的自动前缀缓存能力 |

标签用于候选匹配；这次文本请求实验没有验证图片输入或缓存效果。插件不校验上游是否支持某个标签，拼写或组合不匹配可能导致没有可用候选。

预设模式把最终的 `only/order` 同时写入顶层 `provider` 和 `providerOptions.gateway`，兼容不同路由路径；`has` 与 `inferenceRegion` 只写入 `providerOptions.gateway`。

## 七组实测：2026-10-23（历史，旧结论已失效）

以下参数、结果和耗时保留当日记录，不作为当前固定官方的配置建议。

北京时间 22:16–22:19，使用同一账号、同一网络路径，直接请求 Cline `/api/v1/chat/completions`。全部为同一句短文本，`stream=true`、`stream_options.include_usage=true`、`max_tokens=64`，顺序执行且没有自动重试。表中的模型均带 `cline-pass/` 前缀。

| 组 | 上游模型 | `gateway` 参数 | `finalProvider` | 用时 |
| --- | --- | --- | --- | --- |
| A | `deepseek-v4.1-flash-1017` | 无 | `openai-compatible-private` | 2.290s |
| B | 同 A | 仅 `only=[deepseek]` | `openai-compatible-private` | 7.145s |
| C | 同 A | 仅 `has=[vision,implicit-caching]` | `fireworks` | 2.263s |
| D | 同 A | `only + has` | `deepseek` | 2.189s |
| E | 同 A | 重复 D | `deepseek` | 2.021s |
| F | `deepseek-v4.1-flash`（无后缀） | `only + has` | `openai-compatible-private` | 1.654s |
| G | 同 A | `only + order + has`，顶层 `provider` 同步写 `only/order` | `deepseek` | 1.819s |

七组均 HTTP 200、SSE 含 `[DONE]`、`finish_reason=stop`，没有 SSE JSON 解析错误。G 是按插件请求构造方式直连上游的对照，不等于已部署插件的端到端验证。

这组结果只说明当日账号与时段中 `-1017` 的组合参数命中了官方；2026-11-04 的对照已发现旧组合仍落私有通道。无后缀模型在旧实验中也仍落私有通道，没有验证非流式行为。

## 如何读路由结果

最终提供方看响应的 `provider_metadata.gateway.routing.finalProvider`；历史实验的流式字段位于 `choices[0].delta` 下，插件在日志中显示为「实际上游」。以下细节均描述旧实验：

- C 组的 `canonicalSlug` 已是 `deepseek/deepseek-v4.1-flash-1017`，但 `finalProvider=fireworks`。模型名中的 DeepSeek 不等于最终提供方是 DeepSeek。
- D/E/G 的规划摘要包含 `Provider set restricted to: deepseek`，最终提供方也都是 `deepseek`。这条公开候选路由会应用 `only` 的提供方限制。
- C/D/E/G 的 `modelAttemptCount=2`，但私有模型条目为 `success=false`、`providerAttemptCount=0`、`providerAttempts=[]`；公开模型只有一次提供方尝试。两个模型条目不等于向两家供应商各发送了一次请求。
- `affinity.outcome` 在成功组间也不同。本功能限制单次请求的路由，不代表永久绑定账号或会话。

旧元数据支持“能力条件改变了候选路由，提供方列表进一步限制选择”的解释；网关内部过滤与规划的具体顺序仍属于推断。当前确认官方路由，应同时检查正常生成与 `finalProvider=deepseek`。

## 费用和失败

`cost`、`gatewayCost`、`marketCost`、`inferenceCost` 等是上游返回的费用字段。本轮没有核对账户账单或额度扣减，单凭这些字段不足以认定私有通道免费、公开提供方一定另行收费，或两条通道单价相同。

能力条件与提供方允许列表叠加后，候选过窄可能导致请求失败。测试时也要看 SSE 错误和实际提供方，HTTP 200 本身不代表请求已正常完成。

## 客户端直接传字段

客户端能够传递扩展请求体时，可直接设置区域规则。经插件调用时先选择「客户端优先」；该策略不自动附加区域规则，需要客户端明确发送。默认的「插件配置优先」采用模型配置。

例如 OpenAI Python SDK 的扩展字段写法：

```python
client.chat.completions.create(
    model="deepseek-v4.1-flash-1017",  # 填插件中已配置的客户端模型名称
    messages=[{"role": "user", "content": "只回复 OK"}],
    stream=True,
    extra_body={
        "providerOptions": {
            "gateway": {
                "only": ["deepseek"],
                "has": ["vision", "implicit-caching"],
                "inferenceRegion": {
                    "scope": "global",
                    "providers": {
                        "openai-compatible-private": {
                            "scope": "zone", "geoRegion": "us"
                        }
                    },
                },
            }
        }
    },
)
```

这里是客户端请求，所以需要外层 `providerOptions`；模型自定义 JSON 编辑框则从 `gateway` 那层开始填。预设模式的「客户端优先」按字段保留客户端值，缺少的字段用模型预设补齐；`has: []` 或 `has: null` 原样保留。自定义模式则递归合并整份 `providerOptions`，同名值由客户端覆盖，包括 `null`、`[]`，其他命名空间也参与合并。
