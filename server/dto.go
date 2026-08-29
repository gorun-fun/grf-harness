// dto.go ORS（Open Responses Specification 2026-04-24）wire 类型：请求
// 体、响应资源、输出 item、流式事件、usage 与错误信封。这是 API 的稳定
// 契约层——内部结构（llm.Message 等）演进不直接外泄，映射都在 server 包
// 内完成。gh 特有字段按 ORS §11 扩展规则：自定义 item/事件类型用 grf:
// 冒号前缀，既有对象上加字段用 grf_ 前缀命名。
package server

import "encoding/json"

// 响应状态机（OpenAI 六值枚举，超 ORS 散文但兼容 OpenAI 客户端）。
const (
	statusQueued     = "queued"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusFailed     = "failed"
	statusCancelled  = "cancelled"
	statusIncomplete = "incomplete"
)

// item 类型。
const (
	itemMessage            = "message"
	itemFunctionCall       = "function_call"
	itemFunctionCallOutput = "function_call_output"
	itemReasoning          = "reasoning"
	itemCompaction         = "compaction"
	itemGrfToolCall        = "grf:tool_call"
)

// 请求 input 的 item 类型。
const (
	inItemMessage        = "message"
	inItemFunctionCall   = "function_call"
	inItemFCOutput       = "function_call_output"
	inItemReasoning      = "reasoning"
	inItemGrfInputResult = "grf:input_result"
)

// 暂停类别（与 agent.PauseKind 对应的 wire 值）。
const (
	kindFunction  = "function"
	kindApproval  = "tool_approval"
	kindQuestion  = "question"
	inputRequired = "grf:input_required"
)

// createRequest POST /v1/responses 请求体。未支持的采样参数（temperature
// 等）接受并回显、不生效，见 docs/server.md 的"接受但忽略"清单。
type createRequest struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"` // string 或 item 数组
	Instructions       *string           `json:"instructions"`
	PreviousResponseID string            `json:"previous_response_id"`
	Tools              []toolParam       `json:"tools"`
	ToolChoice         json.RawMessage   `json:"tool_choice"` // "auto"/"none"/"required"/对象
	Include            []string          `json:"include"`
	Metadata           map[string]string `json:"metadata"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
	Stream             bool              `json:"stream"`
	Background         bool              `json:"background"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	MaxToolCalls       *int              `json:"max_tool_calls"`
	SafetyIdentifier   string            `json:"safety_identifier"`
	PromptCacheKey     string            `json:"prompt_cache_key"`
	Truncation         string            `json:"truncation"` // "" | auto | disabled
	Store              *bool             `json:"store"`
	ServiceTier        string            `json:"service_tier"`
	// grf:session 首次请求的会话配置（ORS §11：加字段扩展）。
	GrfSession *grfSessionConfig `json:"grf:session"`
}

// toolParam 请求 tools 数组项（ORS 只保留 function 类型；grf: 前缀 hosted
// 工具配置是扩展，暂未使用——内置 26 个工具默认全量提供）。
type toolParam struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// grfSessionConfig 会话级 gh 配置（首次请求生效，落盘 meta.json）。
type grfSessionConfig struct {
	Sandbox       string   `json:"sandbox,omitempty"`   // off | workspace-write
	FsPolicy      string   `json:"fs_policy,omitempty"` // observe | off
	AutoApprove   *bool    `json:"auto_approve,omitempty"`
	SystemPrompt  string   `json:"system_prompt,omitempty"`
	MaxSteps      int      `json:"max_steps,omitempty"`
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

// inputItem 请求 input 数组的项。
type inputItem struct {
	Type string `json:"type"`
	// message
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string 或部件数组
	Status  string          `json:"status,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output（output 为 JSON 字符串或部件数组）
	Output json.RawMessage `json:"output,omitempty"`
	// reasoning 回传
	Summary []contentPart `json:"summary,omitempty"`
	// grf:input_result（交互应答）
	Answers  map[string]any `json:"answers,omitempty"`
	Approved bool           `json:"approved,omitempty"`
	Feedback string         `json:"feedback,omitempty"`
	Dismiss  bool           `json:"dismiss,omitempty"`
}

// contentPart 内容部件（input 与 output 共用；ORS 部件类型收窄后仅
// text/image 家族）。
type contentPart struct {
	Type     string `json:"type"` // input_text | input_image | output_text | summary_text | refusal
	Text     string `json:"text,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // data URL 或远程 URL
	Detail   string `json:"detail,omitempty"`    // input_image 的 auto/high/low
}

// Item 输出 item（多态字段的并集；输出时按 type 取用，omitempty 压掉空段）。
type Item struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	// message
	Role    string        `json:"role,omitempty"`
	Content []contentPart `json:"content,omitempty"`
	// function_call / grf:tool_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// grf:tool_call 扩展字段（ORS §11：既有 schema 加字段无需类型前缀）
	Input   string `json:"input,omitempty"`
	Output  string `json:"output_text,omitempty"`
	Err     string `json:"err,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Label   string `json:"label,omitempty"`
	// function_call_output
	FCOutput string `json:"fc_output,omitempty"`
	// reasoning
	Summary          []contentPart `json:"summary,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

// IncompleteDetails 未完成原因。
type IncompleteDetails struct {
	Reason string `json:"reason"` // grf:input_required | max_tool_calls
	// grf:input_required 时的交互请求索引（完整参数在对应 output item 的
	// arguments/input 字段里）。
	Requests []IncompleteRequest `json:"grf_requests,omitempty"`
}

// IncompleteRequest 一个待应答的交互请求。
type IncompleteRequest struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // function | tool_approval | question
	Arguments string `json:"arguments"`
}

// ErrorPayload ORS 错误对象。
type ErrorPayload struct {
	Type    string `json:"type"` // invalid_request | not_found | server_error | model_error | too_many_requests
	Code    any    `json:"code"` // 可 null
	Message string `json:"message"`
	Param   any    `json:"param"` // 可 null
}

// errorBody HTTP 错误信封。
type errorBody struct {
	Error ErrorPayload `json:"error"`
}

// Usage 用量对象（ORS 必填结构）。
type Usage struct {
	InputTokens         int             `json:"input_tokens"`
	OutputTokens        int             `json:"output_tokens"`
	TotalTokens         int             `json:"total_tokens"`
	InputTokensDetails  *usageTokenKind `json:"input_tokens_details"`
	OutputTokensDetails *usageTokenKind `json:"output_tokens_details"`
}

type usageTokenKind struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// TextConfig 响应回显的 text 配置。
type TextConfig struct {
	Format    string `json:"format"` // text | json_object | json_schema
	Verbosity string `json:"verbosity,omitempty"`
}

// ResponseResource 响应资源。
type ResponseResource struct {
	ID                 string             `json:"id"`     // resp_<sid>_<seq>
	Object             string             `json:"object"` // "response"
	CreatedAt          int64              `json:"created_at"`
	CompletedAt        *int64             `json:"completed_at"`
	Status             string             `json:"status"`
	IncompleteDetails  *IncompleteDetails `json:"incomplete_details"`
	Error              *ErrorPayload      `json:"error"`
	Model              string             `json:"model"`
	PreviousResponseID string             `json:"previous_response_id"`
	Instructions       *string            `json:"instructions"`
	Output             []Item             `json:"output"`
	Tools              []toolParam        `json:"tools"`
	ToolChoice         any                `json:"tool_choice"`
	Truncation         string             `json:"truncation"`
	ParallelToolCalls  *bool              `json:"parallel_tool_calls"`
	Text               *TextConfig        `json:"text"`
	Temperature        *float64           `json:"temperature"`
	TopP               *float64           `json:"top_p"`
	Reasoning          *json.RawMessage   `json:"reasoning"`
	Usage              *Usage             `json:"usage"`
	MaxOutputTokens    *int               `json:"max_output_tokens"`
	MaxToolCalls       *int               `json:"max_tool_calls"`
	Store              *bool              `json:"store"`
	Background         *bool              `json:"background"`
	ServiceTier        string             `json:"service_tier"`
	Metadata           map[string]string  `json:"metadata"`
	SafetyIdentifier   string             `json:"safety_identifier"`
	PromptCacheKey     string             `json:"prompt_cache_key"`
	// grf 扩展字段
	SessionID string `json:"grf:session_id,omitempty"`
}

// CompactResource /responses/compact 的响应。
type CompactResource struct {
	ID        string `json:"id"`     // cmp_...
	Object    string `json:"object"` // "response.compaction"
	Output    []Item `json:"output"`
	CreatedAt int64  `json:"created_at"`
	Usage     *Usage `json:"usage"`
}

// event 流式事件（SSE data；状态机事件带 response 快照，delta 事件带增量
// 字段；grf 扩展事件复用本结构）。
type event struct {
	Type           string            `json:"type"`
	SequenceNumber int64             `json:"sequence_number"`
	Response       *ResponseResource `json:"response,omitempty"`
	OutputIndex    int               `json:"output_index,omitempty"`
	Item           *Item             `json:"item,omitempty"`
	ItemID         string            `json:"item_id,omitempty"`
	ContentIndex   int               `json:"content_index,omitempty"`
	Part           *contentPart      `json:"part,omitempty"`
	Delta          string            `json:"delta,omitempty"`
	Text           string            `json:"text,omitempty"`
	SummaryIndex   int               `json:"summary_index,omitempty"`
	Error          *ErrorPayload     `json:"error,omitempty"`
	// grf:notice / grf:input_required 扩展字段
	Kind     string             `json:"kind,omitempty"`
	Detail   string             `json:"detail,omitempty"`
	GrfInput *IncompleteDetails `json:"grf_input,omitempty"`
}
