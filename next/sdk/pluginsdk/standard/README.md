# standard：API Key 账号类型的公共库

“账号 = API Key + 基础地址，请求 = 基础地址上按上游协议选的路由，Key 放在请求头里”这一类账号类型，用这个包实现全部平台接口。openai、gemini、relay、azure_openai 四个插件和 anthropic 插件的 `apikey` 类型都是这一类（设计稿 `docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md` 7.3 节）。

它是链接进插件自己进程的 Go 库，不是运行器，也没有清单字段：插件用 Go 写一个 `Type`（通常从厂商预设出发再改几处），交给 `standard.New`，得到的 `*standard.Platform` 就是 `pluginsdk.Platform`。

库里只放每个协议都一样的东西。路径、路径里的模型、请求体的改写、测试请求体、错误分类策略这些协议差异，是 `Type` 里的 Go 值和函数，由 `sdk/pluginsdk/providers/{openai,anthropic,gemini}` 的预设写好。**库里不按厂商分支**：需要 `if openai` 的东西放进预设或插件，不放进这里。

## 接口

```go
package standard

type Type struct {
	ID              string            // 账号类型 ID；New 把它抄进 Spec.AccountType
	Spec            apikey.Spec       // 密钥校验、默认/必填基础地址、去掉的后缀、错误文案
	PassHeaders     []string          // 转发的客户端请求头；清单没写自己的就用 PlatformPassHeaders(平台)
	Auth            Auth              // Key 放在哪
	Headers         []Header          // 每个请求都写的头，按顺序，先于路由的
	Routes          map[string]Route  // 键是上游协议 ID
	DefaultProtocol string            // 没有协议的请求走它；"" 表示拒绝
	FilterFields    []string          // 每个请求体（含 WebSocket 一轮的消息）删掉的路径
	Classify        classify.Policy   // 上游错误分类
	Test            *Test             // 账号测试；nil 回 UNIMPLEMENTED
	Models          *apikey.ModelList // 模型列表；nil 回 UNIMPLEMENTED
}

type Auth struct{ Header, Prefix string } // Key 放在哪个头，Prefix 在 Key 前（"Bearer "）；Header 空 = 不带凭证

type Route struct {
	Path      func(*Request) string                          // 接在基础地址后的路径（已转义）和可选的查询串
	WebSocket bool                                           // GET 的 ws(s) 握手地址、握手头（无 content-type）、请求体是这一轮的消息；其余路由都是 POST
	Model     Model                                          // 上游模型从哪来
	Headers   []Header                                       // 写在类型的头之后
	Edit      func(d *jsonedit.Doc, r *Request)              // 在删掉 FilterFields 之后、同一次 jsonedit 里改请求体；WebSocket 路由不能有
}

type Model struct {
	FromMeta   bool   // 用 meta.model；否则请求体的 "model"（非空字符串），再否则 meta.model
	TrimPrefix string // 去掉两侧空白和这个前缀（"models/"）
	Required   bool   // 没有模型就拒绝
	Missing    string // 拒绝时的消息，默认 "model is required"
}

type Header struct{ Name string; Mode Mode; Value Value }
type Mode int                         // Replace | Keep | Append
func Set(name string, v Value) Header      // 覆盖
func Default(name string, v Value) Header  // 没有才写（客户端传了就用客户端的）
func AppendTo(name string, v Value) Header // 逗号列表里追加；"key=version" 时任何版本的 key 都算已有
type Value func(*Request) string      // 空串 = 不写这个头
func Fixed(s string) Value
func Field(name string) Value         // 账号字段，见 Request.Field
func Path(p string) func(*Request) string

type Test struct {
	Protocol string                   // 用哪个路由（不能是 WebSocket）
	Model    string                   // 控制台没给模型时测它
	Body     func(model string) string
}

type Request struct {
	Account         *pluginv1.Account
	Meta            *pluginv1.RequestMeta // 账号测试、模型列表时为 nil
	APIKey, BaseURL string                // 规范化之后的
	Model           string                // 上游模型；模型列表时为 ""
	Stream          bool                  // 回答是否流式（pluginsdk.RequestStream：meta.stream 或请求体 "stream" 为 true）；账号测试、模型列表时为 false
	Client          map[string]string     // 转发的客户端请求头（小写名、去空白）
}
func (r *Request) Field(name string) string // 先凭证后设置；字符串原样，数字和布尔取 JSON 文本，其余 ""

func New(types ...Type) *Platform   // 配置不一致时 panic（Check 的结果）
func Check(types ...Type) error     // 插件测试里也可以直接调
func PlatformPassHeaders(id string) []string

// *Platform 实现 pluginsdk.Platform、pluginsdk.UpstreamBuilder、pluginsdk.ModelLister：
func (p *Platform) Execute(ctx, *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) // = pluginsdk.ExecuteDefault
func (p *Platform) BuildUpstream(ctx, *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error)
func (p *Platform) ValidateCredentials(ctx, *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error)
func (p *Platform) ClassifyError(ctx, *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error)
func (p *Platform) ClassifyAt(now time.Time, in *pluginv1.ClassifyErrorRequest) *pluginv1.ClassifyErrorResponse
func (p *Platform) BuildTestRequest(ctx, *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error)
func (p *Platform) BuildModelsRequest(ctx, *pluginv1.BuildModelsRequestRequest) (*pluginv1.BuildModelsRequestResponse, error)
```

预设（`sdk/pluginsdk/providers/*`）：

```go
func openai.APIKey(id string) standard.Type    // + 常量 ProtocolChat/Responses/ResponsesWS/Embeddings、ResponsesWebSocketBeta、函数 IncludeStreamUsage
func anthropic.APIKey(id string) standard.Type // 协议常量 ProtocolMessages、ProtocolCountTokens 原有
func gemini.APIKey(id string) standard.Type    // + 常量 ProtocolGenerate/StreamGenerate/CountTokens
```

每次调用返回新的值（新的 map 和切片），插件可以随意改。

## 一次请求怎么构造

`BuildUpstream`（`Execute` 经 `ExecuteDefault` 调它，然后不等待地中继、返回 `host_usage`）：

1. 按 `account.type` 找类型。类型不符报 `unsupported account type "x"`；账号没有类型时，只有一个类型的平台用那个类型，多个类型的拒绝。
2. `Spec.FromAccount`：取 Key 和基础地址（规范化、去后缀、必填、默认值），错误原样返回。
3. 按 `meta.protocol` 找路由（空协议用 `DefaultProtocol`），没有就 `unsupported upstream protocol "x"`。
4. 按 `Route.Model` 取上游模型。
5. 请求体：一次 `jsonedit` 里先删 `FilterFields`，再 `Route.Edit`；两者都没有就不读请求体。
6. 请求头：
   - **默认头**（运营方的透传、覆盖能改）：转发的客户端请求头（只有 `PassHeaders` 里的），然后类型的 `Headers`，然后路由的 `Headers`。WebSocket 路由去掉 `content-type`。
   - **权威头**（运营方改不掉）：只有鉴权头（`Auth.Header`）。
7. URL：基础地址 + `Path`，WebSocket 路由换成 ws/wss。

账号测试走 `Test.Protocol` 的路由，不带客户端请求头，鉴权头与其余头放在同一个 map 里（`BuildTestRequestResponse` 只有一个 map）；模型列表只带类型的头和鉴权，不带 `content-type`。

## 请求体

- **按路径改，不整份解析**：改写都用 SDK 的 `jsonedit`（`sdk/pluginsdk/jsonedit`）。没有改写的请求体原样交回（同一个切片）；改写只动它自己的字节，键顺序、空白、未知字段、`\u` 转义、超出 float64 的数字都不变，只解析一次、最多拷贝一次。`FilterFields` 是几个 `Delete`；`Edit` 在同一个 `Doc` 上接着改，例如 openai 的 `IncludeStreamUsage`：

  ```go
  func IncludeStreamUsage(d *jsonedit.Doc, r *standard.Request) {
  	if r.Stream {
  		d.Set("stream_options.include_usage", []byte("true"))
  	}
  }
  ```

  `Set` 原地替换已有的值，没有就加在所在对象的末尾（连同缺的上层对象）；`Delete` 连同一个分隔逗号删掉成员。
- **非法 JSON、重复键**：核心在调插件之前已经把这两种请求体以 400 拒绝（设计稿 8.1 的结构检查）。库在改写前仍然检查一遍（`jsonedit` 的规则）：要改写的请求体不是合法 JSON，或者 `Set` 路径上的某个键在它所在的对象里出现两次，就返回 `INVALID_ARGUMENT`，什么都不中继（核心按插件不可用换号），而不是发出一个改了一半的请求。路径之外的重复键不归改写管，原样保留；`Delete` 删掉一个路径的每一次出现。路由没有任何改写时，库根本不读请求体。
- **顺序变化（设计稿 12.5）**：`FilterFields` 和 `Edit` 在插件里，排在核心的改写（`claude_max_tokens`、账号的参数覆盖）之后。运营方在参数覆盖里写了一个被过滤的字段，以后会被删掉。预设都不带过滤字段；目前只有 azure_openai 的两个类型自己写了 `service_tier`、`safety_identifier`。
- **流式标志**：交入时为真，交回时仍为真。库不碰 `stream`。`Request.Stream` 是 `meta.stream` 或请求体的 `"stream"` 为 true：只流式的账号类型由核心把 `stream: true` 写进请求体，`meta.stream` 却保留客户端的值，openai 的 `IncludeStreamUsage` 看 `Request.Stream`，否则这种请求拿不到用量。

## 例子一：OpenAI 风格（openai 插件）

`plugins/openai/internal/openai/openai.go`：

```go
// Package openai is the OpenAI API key platform: the openai preset with the
// account's organization and project, forwarding every request field.
package openai

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(APIKey()) }

// APIKey is the apikey type: the preset, plus the organization and project
// an account may set, which replace the client's headers.
func APIKey() standard.Type {
	t := openai.APIKey("apikey")
	t.Headers = append(t.Headers,
		standard.Set("openai-organization", standard.Field("organization")),
		standard.Set("openai-project", standard.Field("project")))
	return t
}
```

`plugins/openai/main.go`：

```go
// Command openai is the sub2api-next OpenAI plugin.
package main

import (
	_ "embed"

	"github.com/Sub2API-Devs/sup2api/next/plugins/openai/internal/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

//go:embed manifest.json
var manifestJSON []byte

func main() {
	pluginsdk.Serve(openai.New(), pluginsdk.WithManifest(manifestJSON))
}
```

`plugins/openai/internal/openai/openai_test.go`：

```go
func TestGolden(t *testing.T) {
	goldentest.Run(t, "openai", standardtest.Golden(New()))
}
```

### WebSocket 的一轮（`openai.responses_ws`，`platform.websocket.v1`）

清单声明 `platform.websocket.v1` 后，核心对 Responses WebSocket 会话的**每一轮**消息调一次 `Execute`（设计稿 4.8）。插件不用为此写任何代码：预设的 `openai.responses_ws` 路由是 `WebSocket: true`，同一个 `BuildUpstream` 给出

| `UpstreamRequest` | 值 |
|---|---|
| `method` | `GET` |
| `url` | 握手地址：`wss://api.openai.com/v1/responses`（基础地址是 http 时为 `ws://`） |
| `headers` | `authorization: Bearer <key>`（权威头） |
| `default_headers` | 转发的客户端请求头，加 `openai-beta`：客户端没给就是 `responses_websockets=2026-02-06`，给了别的值就追加，给了任何版本的 `responses_websockets` 就用客户端的；没有 `content-type` |
| `body` | 这一轮的消息（`FilterFields` 照样作用，没有路由改写） |

`ExecuteDefault` 照常不等待地中继、返回 `host_usage`。核心在会话还没有上游连接时用 URL 和握手头建连，之后每轮只发消息。例：

```go
in := &pluginv1.ExecuteRequest{
	Meta:    &pluginv1.RequestMeta{Protocol: "openai.responses_ws", Model: "gpt-4o-mini", Stream: true},
	Account: &pluginv1.Account{Type: "apikey", CredentialsJson: `{"api_key":"sk-…"}`},
	InboundHeaders: map[string]string{"openai-beta": "assistants=v2"},
	Body:    []byte(`{"type":"response.create","response":{"model":"gpt-4o-mini"}}`),
}
// BuildUpstream(ctx, in) =>
//   GET wss://api.openai.com/v1/responses
//   headers         {"authorization": "Bearer sk-…"}
//   default_headers {"openai-beta": "assistants=v2,responses_websockets=2026-02-06"}
//   body            {"type":"response.create","response":{"model":"gpt-4o-mini"}}
```

## 例子二：Gemini 风格（gemini 插件）

模型在路径里，取自 `meta.model`（去掉 `models/` 前缀，必填），Key 在 `x-goog-api-key`。预设原样就是插件要的：

```go
// Package gemini is the Gemini API key platform: the gemini preset as it is.
package gemini

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(gemini.APIKey("apikey")) }
```

`main.go` 与 openai 的相同，只换 import；测试是 `goldentest.Run(t, "gemini", standardtest.Golden(New()))`。

预设怎么把模型写进路径，可以照着写别的“模型在路径里”的路由：

```go
standard.Route{
	Path:  func(r *standard.Request) string { return "/v1beta/models/" + url.PathEscape(r.Model) + ":generateContent" },
	Model: standard.Model{FromMeta: true, TrimPrefix: "models/", Required: true, Missing: "model is required (path /v1beta/models/{model}:...)"},
}
```

## 例子三：Anthropic 风格（anthropic 插件的 `apikey`，relay 插件）

anthropic 插件已经有进程（模型目录的路由），把平台嵌进现有的 `Plugin`：

```go
package anthropic

import (
	"context"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// Plugin is the anthropic plugin: the apikey platform (pluginsdk.Platform)
// and the model catalog route (pluginsdk.HTTP).
type Plugin struct {
	*pluginsdk.Router
	*standard.Platform
	host pluginsdk.Host
}

// New returns the plugin.
func New() *Plugin {
	p := &Plugin{Router: pluginsdk.NewRouter(), Platform: standard.New(APIKey())}
	p.Handle("GET", "/models", p.listModels)
	return p
}

// APIKey is the apikey type: the anthropic preset as it is (every request
// field is forwarded).
func APIKey() standard.Type { return anthropic.APIKey("apikey") }

// Init implements pluginsdk.Initializer.
func (p *Plugin) Init(_ context.Context, h pluginsdk.Host) error {
	p.host = h
	return nil
}
```

`main.go` 不变（`pluginsdk.Serve(anthropic.New(), pluginsdk.WithManifest(manifestJSON))`）。

relay 是同一个预设加三处差异：

```go
// Package relay is the platform of Anthropic-compatible relays.
package relay

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// New returns the platform of the relay_key account type.
func New() *standard.Platform { return standard.New(RelayKey()) }

// RelayKey is the anthropic preset on the relay's own address: the base URL
// is required and has no default, a request must name its protocol, the
// plugin forwards every field, and only the two headers the manifest passes
// reach the relay.
func RelayKey() standard.Type {
	t := anthropic.APIKey("relay_key")
	t.Spec.DefaultBaseURL, t.Spec.RequireBaseURL = "", true
	t.Spec.Texts.BaseURLRequired = "Base URL of the relay is required / 请填写中转站的 Base URL"
	t.DefaultProtocol = ""
	t.PassHeaders = []string{"anthropic-version", "anthropic-beta"}
	return t
}
```

测试：`goldentest.Run(t, "anthropic", standardtest.Golden(standard.New(APIKey())))`、`goldentest.Run(t, "relay", standardtest.Golden(New()))`。

azure_openai 的两个类型从 openai 预设出发（`api-key` 鉴权、必填的资源地址、去掉 `/openai/v1` 和 `/openai`、不校验 Key 前缀、按部署名的经典路由带 `api-version`、没有模型列表、十个客户端请求头），配置在 `plugins/azure-openai/internal/azureopenai/azureopenai.go`，验收断言在同一插件的 `platform_test.go`。

## 测试

- `standardtest.Golden(p)` 把一个 `*standard.Platform` 接到 `goldentest.Run`：`Execute` 是 `BuildUpstream`（`ExecuteDefault` 中继的就是它），其余四类是同名方法，分类用样例的时钟（`ClassifyAt`）。
- 想连同“恰好一次中继、不等待、返回 `host_usage`”一起验，用假宿主：`pluginsdktest.Start(t, p, …)` 后 `h.Execute(ctx, in).Relayed(t)`（本包 `platform_test.go` 的 `TestExecute`）。
- `standard.Check(t)`：类型配置能通过检查。

本包自己的验收只测库和预设本身：`platform_test.go`、`body_test.go`（请求体逐字节保持、改写只动自己的字节、非法 JSON 与重复键），`providers/gemini/golden_test.go` 用原样的 gemini 预设回放 gemini 的样例。插件的类型配置不在 SDK 里复制：1498 条冻结样例（openai 441、anthropic 343、gemini 371、relay 343）由各插件用自己的构造函数回放，azure_openai 的断言在它自己的 `platform_test.go`，CI 测试每个 `plugins/*` 模块。
