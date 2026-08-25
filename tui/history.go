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

func loadHistory() *lineHistory {
	h := &lineHistory{}
	if home, err := os.UserHomeDir(); err == nil {
		if v := os.Getenv("GRF_HOME"); v != "" {
			home = v
		}
		if data, err := os.ReadFile(filepath.Join(home, "history")); err == nil {
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
		if home, err := os.UserHomeDir(); err == nil {
			if v := os.Getenv("GRF_HOME"); v != "" {
				home = v
			}
			path := filepath.Join(home, "history")
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
