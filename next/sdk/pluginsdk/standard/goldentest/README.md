# goldentest：API Key 平台插件的冻结黄金样例

这里是 openai、anthropic、gemini、relay 四个插件的验收依据，共 1498 条（441、343、371、343）。它们是当年四个代码插件改成声明式之前录下的回答，由 `sdk/upstream/testdata/golden` 转换而来（`docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md` 7.3 节，任务包 W2）。

转换后的样例不依赖声明式引擎，也不依赖 `BuildUpstreamRequest` 的消息：引擎（`sdk/upstream`）和旧的黄金文件删除之后，这个包原样留下，供 `sdk/pluginsdk/standard` 和五个插件的改写（W4、W10a 到 W10e）使用。

**样例是冻结的**：不增、不删、不改。发现样例与新实现不一致时，改的是实现；确实合理的差异用 `Except` 逐条声明（见下）。

## 文件

| 路径 | 内容 |
|---|---|
| `testdata/golden/<plugin>.json` | 转换后的样例，随包内嵌（`go:embed`），插件模块 import 这个包就能用，不需要相对路径 |
| `goldentest.go` | 样例的结构、读取、回放 |
| `MANIFEST-ASSERTIONS.md` | 五份 `manifest_test.go` 里依赖引擎的断言清单（azure_openai 没有黄金文件，以它为准） |
| `sdk/upstream/golden_convert_test.go` | 一次性的转换程序和忠实性证明，**随引擎一起删除** |

位置与设计稿原文（`sdk/pluginsdk/standard/testdata/golden`）差一层目录，原因：样例要被别的模块（`plugins/*`）的测试读到，只能由一个非测试包内嵌后导出，而 `go:embed` 只能内嵌本包目录之下的文件；放进 `standard` 包本身则会把 1.2 MB 的样例链接进每个插件的二进制。

## 样例的结构

一个文件：

```json
{
  "plugin": "openai",
  "account_type": "apikey",
  "pass_headers": ["openai-organization", "..."],
  "cases": [ ... ]
}
```

`pass_headers` 是转换时这个账号类型能收到的客户端请求头（清单的 `passHeaders`，没写就是平台的）。样例里的客户端请求头故意比它多（`authorization`、`cookie`、`x-api-key` 等），多出来的不允许出现在上游请求里。

一条样例：

| 字段 | 含义 |
|---|---|
| `name` | 与旧黄金文件里的名字逐字相同（所以 `execute` 样例的名字仍以 `build` 开头），文件内唯一 |
| `kind` | `execute`、`test`、`models`、`validate`、`classify` |
| `now` | 只有 `classify` 有：分类必须使用的时钟（RFC 3339） |
| `input` | 输入，见下 |
| `want` | 期望的回答；与 `error` 恰好有一个 |
| `error` | 期望失败：`message` 参与比较，`code` 只作记录不比较 |
| `note` | 只在期望值不是当年录下的回答时出现，说明原因（共 49 条，见“与录制不同的样例”） |

### `execute`：一次网关请求的上游请求

旧样例是“声明的字段进、URL 加请求头加补丁出”；新插件拿到的是完整请求体，交出的是最终字节，所以这一类换了结构。

`input`：

```json
{
  "account": {"id": 42, "type": "apikey", "credentials_json": "{...}", "settings_json": "{...}"},
  "meta": {"protocol": "openai.chat", "model": "golden-model", "stream": true},
  "client_headers": {"user-agent": "..."},
  "body": "{\"model\":\"field-model\",\"stream\":true}"
}
```

- `account` 缺失就是“没有账号”的样例；`type` 为空表示核心没带类型。
- `meta` 只有样例矩阵变化的三项：上游协议、模型、是否流式。
- `body` 是完整请求体的原文（字符串，逐字节）。

`want`：

```json
{
  "method": "POST",
  "url": "https://upstream.example.com/v1/chat/completions",
  "headers": {"authorization": "Bearer sk-golden-key-0123456789"},
  "default_headers": {"content-type": "application/json", "user-agent": "..."},
  "body": "{\"model\":\"field-model\",\"stream\":true,\"stream_options\":{\"include_usage\":true}}",
  "upstream_model": "field-model"
}
```

- `headers` 是**权威头**（写在运营方的请求头设置之后）：只有携带凭证的那一个头（openai 是 `authorization`，anthropic 和 relay 是 `x-api-key`，gemini 是 `x-goog-api-key`）。
- `default_headers` 是**默认头**（写在运营方设置之前，运营方能改）：其余全部，包括转发的客户端请求头。划分依据是今天核心的分层（`gateway/dispatch.go` 的 `forwardBuilt`：声明式类型的头在透传和覆盖之下，鉴权头最后再写一遍），见设计稿 4.5 节。
- `body` 是最终请求体的原文，**逐字节比较**。WebSocket 路由（`openai.responses_ws`）上它是这一轮的消息，方法是 `GET`，地址是 `ws(s)://`，没有 `content-type`。
- 请求头的名字不区分大小写；同一个名字以不同大小写出现两次算错。

### 请求体是怎么合成的

旧样例没有请求体，只有核心从请求体里读出来交给插件的字段（四个插件都只有 `model`）。转换按下面两条合成“核心能读出这份输入的最小请求体”：

1. 录下的每个字段写到它的路径上：`{"model":"field-model"}`；字段值原样保留（样例里有 `123`、`""`、`"models/field-model"`）；没有字段就是 `{}`。
2. `meta.stream` 为真、并且 `meta.protocol` 对应的内置端点声明了 `streamPath` 时，在那个路径上写 `true`（核心就是从这个标志得到 `meta.stream` 的）。实际涉及 `openai.chat`、`openai.responses`、`anthropic.messages` 三个协议，路径都是 `stream`。

然后把录下的补丁按顺序应用上去，得到期望的最终请求体。四个文件里出现过的补丁只有一种：`openai.chat` 流式时 `stream_options.include_usage = true`（28 条）。其余样例的期望请求体与输入逐字节相同，也就是插件必须原样交回。

两点要知道：

- 请求体里的 `model` 与 `meta.model` 故意不同（`field-model` 对 `golden-model`），样例借此固定“请求体里的模型优先，gemini 的路由用 `meta` 的”。真实请求里两者相同。
- 协议为空（走默认协议）且 `meta.stream` 为真的 openai 样例有 14 条：请求体里没有 `stream` 标志（规则 2 查不到协议），期望里仍有 `stream_options`。它们固定的是“流式补丁看的是 `meta.stream`，不是请求体”。

### 其余四类

`test`、`models`、`validate`、`classify` 的 RPC 不变（设计稿 4.8 节），`input` 和 `want` 就是请求、应答消息的 proto JSON（proto 字段名），内容与旧黄金文件相同。比较时把实现的回答编码成同样的 JSON 再逐字节比。

### 错误

`error.message` 是去掉插件自己包装之后的消息：开头的 `account N: ` 和嵌套的 `rpc error: code = X desc = ` 两侧都先去掉再比（`Unwrap`）。`error.code` 是录制时插件回答的 gRPC 码，不比较：核心对这些调用的失败一视同仁，而且今天的码已经与录制的不同（anthropic 插件一律回 `FailedPrecondition`，公共的账号检查回 `InvalidArgument`）。

## 与录制不同的样例（49 条）

1498 条全部转换，没有跳过。其中 49 条的期望值是**今天的行为**而不是当年录下的回答，每条带 `note`：

| 条数 | 哪些 | 原因 |
|---|---|---|
| 40 | relay 成功的 `test` | 录制时 `model`、`usage_protocol` 是空的；relay 改成声明式之后账号测试与其他类型一样带上这两项（旧 `TestGolden` 允许的唯一偏差） |
| 9 | 账号类型不是本插件类型的调用：openai 的 `models` 1 条；gemini 的 `execute` 3 条、`test` 4 条、`models` 1 条 | 当年这两个插件对别的类型的账号照常作答（其中 gemini 的一条先报了协议错误）；引擎绑定在一个账号类型上，一律拒绝，消息是 `unsupported account type "oauth"`，与 anthropic、relay 一直以来的回答相同。旧 `TestGolden` 跳过全部 32 条类型不符的样例；这里其余 23 条按录制转换，这 9 条写成今天的回答。核心不会把别的类型的账号交给插件，这类输入在线上不出现 |

## 已知的顺序例外：声明的过滤字段和 `paramOverride`

设计稿 7.3 节末尾、12.5 节：今天声明的 `filterFields` 和声明的 `paramOverride` 在核心里**先于**运营方的改写（`claude_max_tokens`、账号的 `param_override`）执行（`gateway/param_override.go` 的 `rewriteBody`）；改成代码插件后这一层在插件里，排在运营方的改写**之后**。运营方的参数覆盖写入了一个被声明过滤的字段时：今天那个值保留，以后被插件删掉。

对这批样例的影响：**没有一条样例会因此不同。** 原因有三：

- 样例录制用的类型都是 `filterFields: []`、没有 `paramOverride`（`sdk/upstream/golden_test.go` 的 `goldenTypes`），样例只测请求的构造。
- 样例的请求体只有 `model`、`stream`，不含任何会被过滤的字段。所以即使被测的类型带着预设的过滤字段，结果也相同。
- 样例不经过核心的改写流程，没有“先后”可言。

所以不要用这条例外给样例开 `Except`。它影响的是样例之外的行为，实际范围（转换时逐份核对了三个预设和五份清单）：

| 类型 | 声明的过滤字段 | 声明的 `paramOverride` |
|---|---|---|
| openai/`apikey`、anthropic/`apikey`、gemini/`apikey`、relay/`relay_key` | 无（清单当年写的是 `filterFields: []`） | 无 |
| azure_openai/`azure_v1`、`azure_deployment` | `service_tier`、`safety_identifier`（插件的 `azure()` 里写明） | 无 |
| 预设本身 | 无（预设不带过滤字段，azure_openai 自己写这两个） | 无 |

也就是说，今天这条顺序变化只在 azure_openai 的两个类型上可见：运营方在账号的参数覆盖里设置 `service_tier` 或 `safety_identifier` 时，今天发给上游，改写之后被插件删掉。W4、W10d 要把它写进 CONTRACTS 新章节和 CHANGELOG。内置的 `claude_max_tokens` 只写 `max_tokens`，不受影响。

另外，线上 openai 清单比样例用的类型多两个由账号设置决定的头（`openai-organization`、`openai-project`）。样例里的账号没有这两项设置，模板缺值时不写头，所以用线上的类型配置回放，结果相同（`MANIFEST-ASSERTIONS.md` 的 openai 一节）。

## 怎么用

```go
import "github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"

func TestGolden(t *testing.T) {
	p := openai.New() // 被测的实现
	goldentest.Run(t, "openai", goldentest.Subject{
		Execute: func(ctx context.Context, in *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
			// 用 in.Account.Proto()、in.Meta.Proto()、in.ClientHeaders、[]byte(in.Body)
			// 调插件的 Execute（假宿主），把它交给中继的请求填进 UpstreamRequest。
		},
		Test:     func(ctx context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) { ... },
		Models:   func(ctx context.Context, in *pluginv1.BuildModelsRequestRequest) (*pluginv1.BuildModelsRequestResponse, error) { ... },
		Validate: func(ctx context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) { ... },
		Classify: func(ctx context.Context, now time.Time, in *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) { ... },
	})
}
```

接口：

```go
func Run(t *testing.T, plugin string, s Subject, opts ...Option) // 回放一个文件的全部样例，逐条报告差异
func Except(name, reason string) Option                          // 声明一条已知的差异
func Load(plugin string) (*File, error)                          // 只读样例，自己遍历
func Plugins() []string
func (c *Case) Check(ctx context.Context, s Subject) []string    // 回放一条，返回差异
func (c *Case) Execute() (*ExecuteInput, *UpstreamRequest, error)
func Unwrap(message string) string
```

规则：

- `Subject` 的五个函数都要给。缺一个，那一类的每条样例都报错，不会悄悄跳过。
- `Classify` 收到样例的时钟 `now`，实现必须用它而不是系统时间（冷却到期时间在期望值里）。
- `Execute` 的适配器负责把 `pass_headers`（`File.PassHeaders`）交给被测实现，如果实现需要的话；`client_headers` 原样传入，过滤是被测实现的事。
- `Except(name, reason)` 声明的样例必须**真的不同**：声明了却与样例一致、或者名字不存在，测试失败。这样例外清单不会比它的理由活得久。

## 转换是否忠实：两个测试

在 `sdk/upstream/golden_convert_test.go`，文件开头标着随引擎删除：

- `TestGoldenConversion`：不调用引擎，只从录下的回答转换，要求提交的文件与转换输出逐字节相同，并核对条数（441、343、371、343）和带 `note` 的条数（1、0、8、40）。转换时还检查每条输入“转回去”仍是原输入（账号、元信息、字段、客户端请求头一项不丢）。重新生成：`SUB2API_GOLDEN_CONVERT=write go test -run TestGoldenConversion ./upstream/`（在 `next/sdk` 下）。
- `TestConvertedGoldenReplay`：把转换后的每一条经 `goldentest.Run` 交给今天的引擎，按核心的用法回放（从请求体读字段、`Build`、把补丁应用到请求体、把请求头分成声明的和凭证的），1498 条全部一致。

两者一个出自录制、一个出自引擎，互相独立；旧的 `TestGolden` 不动，继续直接比较旧文件。
