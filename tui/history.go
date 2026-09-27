// history.go 输入历史（~/.grf/history 持久化，上限 500 条）。
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type lineHistory struct {
	items []string
	idx   int
	saved string
}

const historyLimit = 500

// historyPath 输入历史落点（home 下名为 history 的文件，GRF_HOME 覆盖；
// home 取不到时返回空串表示持久化不可用）。
func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if v := os.Getenv("GRF_HOME"); v != "" {
		home = v
	}
	return filepath.Join(home, "history")
}

func loadHistory() *lineHistory {
	h := &lineHistory{}
	if path := historyPath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			for _, ln := range strings.Split(string(data), "\n") {
				if ln != "" {
					h.items = append(h.items, ln)
				}
			}
			if len(h.items) > historyLimit {
				h.items = h.items[len(h.items)-historyLimit:]
			}
		}
	}
	h.start()
	return h
}

func (h *lineHistory) start() { h.idx = len(h.items); h.saved = "" }

func (h *lineHistory) add(line string) {
	if line == "" {
		return
	}
	if n := len(h.items); n == 0 || h.items[n-1] != line {
		h.items = append(h.items, line)
		if len(h.items) > historyLimit {
			h.items = h.items[len(h.items)-historyLimit:]
		}
		if path := historyPath(); path != "" {
			if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
				if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					fmt.Fprintln(f, line)
					f.Close()
				}
			}
		}
	}
	h.start()
}

func (h *lineHistory) up(cur string) string {
	if h.idx == 0 {
		return cur
	}
	if h.idx == len(h.items) {
		h.saved = cur
	}
	h.idx--
	return h.items[h.idx]
}

func (h *lineHistory) down(cur string) string {
	if h.idx >= len(h.items) {
		return cur
	}
	h.idx++
	if h.idx == len(h.items) {
		return h.saved
	}
	return h.items[h.idx]
}
