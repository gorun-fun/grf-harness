// spec_test.go spec schema 与实现参数的交叉校验：对每个内置工具，实现侧
// 参数 struct 的 json tag 键集必须与 tools.json 对应工具的
// parameters.properties 键集完全一致（双向差集为空）——spec 新增字段漏
// 实现（或实现字段未进 spec）时在此报警。
//
// 实现侧来源分两类：
//   - 本包各工具的参数 struct 已提取为具名类型（如 readArgs），测试直接
//     反射 json tag（argSamples）；
//   - 其余工具的 handler 不在本包可反射范围内，用硬编码 tag 清单
//     （hardcodedArgTags）：orchestration 工具（goal/manager/ralph/
//     workflow 系列）的 handler 在 agent 包；exit_plan_mode/
//     enter_plan_mode/memory 的实现文件（plan.go/memory.go）按约定冻结
//     不改，同样以清单维护。
//
// 只比对 properties 键集，不比对 required（实现侧对各参数是否硬校验没有
// 统一可反射的口径；bash 的 spec required 已收敛为仅 command，与其执行器
// 的实际校验一致）。
package tools

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// argSamples 本包内可反射的实现参数样本：工具名 -> 参数 struct 零值。
// 本包新增工具时，把它的具名参数 struct 登记进来。
var argSamples = map[string]any{
	"read":              readArgs{},
	"write":             writeArgs{},
	"edit":              editArgs{},
	"glob":              globArgs{},
	"grep":              grepArgs{},
	"read_image":        readImageArgs{},
	"bash":              bashArgs{},
	"job_output":        jobOutputArgs{},
	"job_kill":          jobKillArgs{},
	"todo_write":        todoWriteArgs{},
	"web_search":        webSearchArgs{},
	"set_policy":        setPolicyArgs{},
	"skill":             skillArgs{},
	"ask_user_question": askUserArgs{},
}

// hardcodedArgTags 无法反射的 handler 的 json tag 清单（见文件头说明）。
// job_list / get_goal 无参数（spec 亦无 properties）。
var hardcodedArgTags = map[string][]string{
	// plan.go / memory.go（文件冻结，以清单代反射）
	"enter_plan_mode": {"reason"},
	"exit_plan_mode":  {"plan"},
	"memory":          {"action", "scope", "key", "content"},
	// 本包实现但无参数 struct（handler 不解码参数）
	"job_list": {},
	// agent/goals.go
	"create_goal": {"objective", "max_goal_rounds"},
	"get_goal":    {},
	"update_goal": {"goal_id", "revision", "action", "objective", "max_goal_rounds", "blocked_reason"},
	// agent/manager.go
	"subagent":        {"description", "prompt", "run_in_background"},
	"subagent_fork":   {"description", "prompt", "run_in_background"},
	"send_message":    {"subagent_id", "message"},
	"interrupt_agent": {"agent_id"},
	"list_agents":     {"scope"},
	// agent/ralph.go / agent/workflow.go
	"ralph":    {"objective", "maxRounds"},
	"workflow": {"script", "meta", "args"},
}

// jsonTagNames 反射取出 struct 各字段的 json tag 名（无名或 "-" 跳过），
// 返回已排序。
func jsonTagNames(v any) []string {
	var tags []string
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			tags = append(tags, name)
		}
	}
	sort.Strings(tags)
	return tags
}

// specPropNames 取 spec 定义里 parameters.properties 的键集（已排序）。
func specPropNames(t *testing.T, name string) []string {
	t.Helper()
	def, ok := SpecDef(name)
	if !ok {
		t.Fatalf("spec missing tool %s", name)
	}
	var names []string
	// specDefs 由 JSON 反序列化而来，嵌套对象必为 map[string]any
	props, _ := def.Parameters["properties"].(map[string]any)
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// keyDiff 求有序键集 a 相对 b 的差集（a 有而 b 没有）。
func keyDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// TestSpecSchemaMatchesImplArgs 逐工具比对实现 json tag 键集与 spec
// properties 键集：双向差集都必须为空；且 spec 的每个工具都有实现来源、
// 实现侧不得登记 spec 之外的名字。
func TestSpecSchemaMatchesImplArgs(t *testing.T) {
	// 覆盖完整性：两类来源合起来必须恰好覆盖全部 spec 工具
	specNames := SpecNames()
	if got := len(argSamples) + len(hardcodedArgTags); got != len(specNames) {
		t.Fatalf("实现参数来源数 %d != spec 工具数 %d（新工具须同时登记 argSamples/hardcodedArgTags）",
			got, len(specNames))
	}
	for name := range argSamples {
		if _, dup := hardcodedArgTags[name]; dup {
			t.Fatalf("%s 同时登记在 argSamples 与 hardcodedArgTags", name)
		}
		if _, ok := SpecDef(name); !ok {
			t.Fatalf("argSamples 含 spec 之外的工具 %s", name)
		}
	}
	for name := range hardcodedArgTags {
		if _, ok := SpecDef(name); !ok {
			t.Fatalf("hardcodedArgTags 含 spec 之外的工具 %s", name)
		}
	}
	for _, name := range specNames {
		var impl []string
		if s, ok := argSamples[name]; ok {
			impl = jsonTagNames(s)
		} else if tags, ok := hardcodedArgTags[name]; ok {
			impl = slices.Clone(tags)
			sort.Strings(impl)
		} else {
			t.Fatalf("%s 没有实现侧参数来源（登记进 argSamples 或 hardcodedArgTags）", name)
		}
		spec := specPropNames(t, name)
		if only := keyDiff(spec, impl); len(only) > 0 {
			t.Errorf("%s: spec 有而实现没有的字段 %v（spec 新增字段漏实现？）", name, only)
		}
		if only := keyDiff(impl, spec); len(only) > 0 {
			t.Errorf("%s: 实现有而 spec 没有的字段 %v", name, only)
		}
	}
}
