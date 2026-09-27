// fallback_tags.go 正文内工具调用标签的共享事实：两种回退格式
// （<tool_call> XML 风格 / DeepSeek DSML 标签风格，解析器分别在
// toolcall_fallback.go 与 dsml.go）与流式显示过滤（stream_display.go）
// 共用的标记常量与变体归一正则，集中一处避免三处各写一套。
package llm

import "regexp"

// toolCallTag 正文内 XML 风格工具调用标签的统一前缀（开/闭标签字面量
// 均以它为前缀派生）。
const toolCallTag = "<tool_call"

// dsmlBar DSML 标记用的全角竖线（U+FF5C），不是 ASCII |。
const dsmlBar = "｜"

// dsmlCore DSML 标记内核（不含尖括号）。
const dsmlCore = dsmlBar + dsmlBar + "DSML" + dsmlBar + dsmlBar

// dsmlMarker DeepSeek DSML 开标签标记（含起始 "<"）。
const dsmlMarker = "<" + dsmlCore

// dsmlCloseMark DSML 闭合标签标记 "</｜｜DSML｜｜"。
const dsmlCloseMark = "</" + dsmlCore

// markerVariantPattern 标记分隔符变体的正则源：竖线可能是 ASCII | 与
// 全角 ｜ 的任意组合、夹带空格（如 <||DSML||tool_call>）。dsml.go 用
// 归一正则把变体归一为标准全角形式；toolcall_fallback.go 的宽松闭合
// 匹配也引用同一 pattern 拼装，须容忍同一套变体。
const markerVariantPattern = `[｜|]\s*[｜|]\s*DSML\s*[｜|]\s*[｜|]`

// reMarkerVariant markerVariantPattern 编译后的归一正则，供
// extractDSMLCalls 把变体标记替换为标准全角形式。
var reMarkerVariant = regexp.MustCompile(markerVariantPattern)
