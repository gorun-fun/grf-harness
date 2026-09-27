// todo.go todo_write 工具与任务列表状态（整表替换语义）；UI 通过
// NotifyEvent("todos", json) 展示。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Todo 单条任务事项。
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// TodoState 任务列表状态（整表替换语义）。
type TodoState struct {
	Items    []Todo
	OnChange func([]Todo)
}

// todoWriteArgs todo_write 工具参数（具名类型供 spec 交叉校验测试反射
// json tag）。
type todoWriteArgs struct {
	Todos []Todo `json:"todos"`
}

// RegisterTodoWrite 注册 todo_write。state 由 agent 持有。
func RegisterTodoWrite(r *Registry, state *TodoState) {
	r.Register("todo_write", func(ctx context.Context, args string) (Result, error) {
		var a todoWriteArgs
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		seen := map[string]bool{}
		for _, td := range a.Todos {
			switch td.Status {
			case "pending", "in_progress", "completed":
			default:
				return Result{}, fmt.Errorf("invalid status %q (use pending|in_progress|completed)", td.Status)
			}
			if strings.TrimSpace(td.Content) == "" {
				return Result{}, fmt.Errorf("todo content must not be empty")
			}
			if seen[td.Content] {
				return Result{}, fmt.Errorf("duplicate todo content %q", td.Content)
			}
			seen[td.Content] = true
		}
		state.Items = a.Todos
		if state.OnChange != nil {
			state.OnChange(a.Todos)
		}
		return TextResult(fmt.Sprintf("todo list updated (%d item%s)", len(a.Todos), plural(len(a.Todos)))), nil
	})
}

// MarshalTodos 序列化 todo 列表（事件通知用）。
func MarshalTodos(todos []Todo) string {
	data, _ := json.Marshal(todos)
	return string(data)
}
