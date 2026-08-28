// fallback_tags.go 正文内工具调用标签的共享事实：两种回退格式
//（<tool_call> XML 风格 / DeepSeek DSML 标签风格，解析器分别在
// toolcall_fallback.go 与 dsml.go）与流式显示过滤（stream_display.go）
// 共用的标记常量与变体归一正则，集中一处避免三处各写一套。
package llm

import "regexp"

// toolCallTag 正文内 XML 风格工具调用标签的统一前缀（开/闭标签字面量
// 均以它为前缀派生）。
const toolCallTag = "<tool_call"

// dsmlMarker DeepSeek DSML 标签标记。注意：竖线是全角字符 ｜（U+FF5C），
// 不是 ASCII |。
const dsmlMarker = "<｜｜DSML｜｜"

// reMarkerVariant 标记分隔符变体：竖线可能是 ASCII | 与全角 ｜ 的任意
// 组合、夹带空格（如 <||DSML||tool_call>）。dsml.go 用它把变体归一为
// 标准全角形式；toolcall_fallback.go 的宽松闭合匹配也须容忍同一套变体。
var reMarkerVariant = regexp.MustCompile(`[｜|]\s*[｜|]\s*DSML\s*[｜|]\s*[｜|]`)
