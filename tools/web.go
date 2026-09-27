// web.go web_search 工具：1-4 个查询合并检索。无需 API key：DuckDuckGo
// Instant Answer（摘要答案）+ lite/html 端点抓取（来源 URL 与标题片段），
// 纯标准库实现（http + regexp + json）。返回可选 answer + 来源列表。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var webClient = &http.Client{Timeout: 30 * time.Second}

// SearchHit 一条搜索结果。
type SearchHit struct {
	Title   string
	URL     string
	Snippet string
}

// WebSearch 执行检索：合并去重多个查询的结果，bestAnswer 为可选摘要。
func WebSearch(ctx context.Context, queries []string) (bestAnswer string, hits []SearchHit, err error) {
	if len(queries) == 0 {
		return "", nil, fmt.Errorf("queries array is required (1-4 items)")
	}
	if len(queries) > 4 {
		queries = queries[:4]
	}
	seen := map[string]bool{}
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		if ans, ok := ddgAnswer(ctx, q); ok && bestAnswer == "" {
			bestAnswer = ans
		}
		for _, h := range ddgSearch(ctx, q) {
			u := strings.TrimSpace(h.URL)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 && bestAnswer == "" {
		return "", nil, fmt.Errorf("no results found for: %s (the search backend may be unreachable or rate-limited; report this and continue without web data)", strings.Join(queries, "; "))
	}
	return bestAnswer, hits, nil
}

// ddgAnswer DuckDuckGo Instant Answer 摘要。
func ddgAnswer(ctx context.Context, q string) (string, bool) {
	u := "https://api.duckduckgo.com/?q=" + url.QueryEscape(q) + "&format=json&no_html=1&skip_disambig=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", ua())
	resp, err := webClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false
	}
	var out struct {
		AbstractText string `json:"AbstractText"`
		Answer       string `json:"Answer"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", false
	}
	if s := strings.TrimSpace(out.AbstractText); s != "" {
		return s, true
	}
	if s := strings.TrimSpace(out.Answer); s != "" && !strings.ContainsAny(s, "{}\")(") {
		return s, true
	}
	return "", false
}

// ddgSearch lite → html 两级抓取。
func ddgSearch(ctx context.Context, q string) []SearchHit {
	if hits := ddgLite(ctx, q); len(hits) > 0 {
		return hits
	}
	return ddgHTML(ctx, q)
}

// ddgPageLimit DDG 结果页响应体读取上限。合并前 lite 取 2MB、html 取
// 4MB，统一取较宽的 4MB：lite 表格版页面实际远小于 2MB，放宽无损；html
// 版标记冗余更大，按 2MB 截断会丢尾部结果。
const ddgPageLimit = 4 << 20

var liteLinkRe = regexp.MustCompile(`(?s)<a[^>]+class="result-link"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
var liteSnipRe = regexp.MustCompile(`(?s)class="result-snippet"[^>]*>(.*?)</td>`)

var htmlLinkRe = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
var htmlSnipRe = regexp.MustCompile(`(?s)class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)

// ddgFetch 抓取并解析一个 DDG POST 结果端点（lite/html 两版除 URL 与
// 提取正则外逐行相同，故合并）：同一套表单 POST + link/snippet 正则提取。
func ddgFetch(ctx context.Context, q, endpoint string, linkRe, snipRe *regexp.Regexp) []SearchHit {
	form := url.Values{"q": {q}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ua())
	resp, err := webClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, ddgPageLimit))
	if err != nil {
		return nil
	}
	page := string(data)
	var hits []SearchHit
	links := linkRe.FindAllStringSubmatch(page, -1)
	snips := snipRe.FindAllStringSubmatch(page, -1)
	for i, m := range links {
		u := cleanDDGURL(m[1])
		title := htmlText(m[2])
		if u == "" || title == "" {
			continue
		}
		h := SearchHit{Title: title, URL: u}
		if i < len(snips) {
			h.Snippet = htmlText(snips[i][1])
		}
		hits = append(hits, h)
	}
	return hits
}

// ddgLite lite.duckduckgo.com/lite 表格版结果。
func ddgLite(ctx context.Context, q string) []SearchHit {
	return ddgFetch(ctx, q, "https://lite.duckduckgo.com/lite/", liteLinkRe, liteSnipRe)
}

// ddgHTML html.duckduckgo.com/html 结果页。
func ddgHTML(ctx context.Context, q string) []SearchHit {
	return ddgFetch(ctx, q, "https://html.duckduckgo.com/html/", htmlLinkRe, htmlSnipRe)
}

func ua() string {
	return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
}

// cleanDDGURL 还原 DDG 跳转链接（//duckduckgo.com/l/?uddg=<encoded>）。
func cleanDDGURL(u string) string {
	u = htmlText(u)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if strings.Contains(u, "uddg=") {
		if p, err := url.Parse(u); err == nil {
			if raw := p.Query().Get("uddg"); raw != "" {
				if dec, err := url.QueryUnescape(raw); err == nil {
					return dec
				}
			}
		}
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return ""
	}
	// 过滤广告跳转
	if strings.Contains(u, "duckduckgo.com/y.js") {
		return ""
	}
	return u
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// htmlText 去 HTML 标签并还原常见实体。
func htmlText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#x27;", "'", "&#39;", "'", "&nbsp;", " ", "&#x2F;", "/",
	)
	s = r.Replace(s)
	return strings.TrimSpace(s)
}

// webSearchArgs web_search 工具参数（具名类型供 spec 交叉校验测试反射
// json tag）。
type webSearchArgs struct {
	Queries []string `json:"queries"`
}

// RegisterWebSearch 注册 web_search 工具。
func RegisterWebSearch(r *Registry) {
	r.Register("web_search", func(ctx context.Context, args string) (Result, error) {
		var a webSearchArgs
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if len(a.Queries) == 0 {
			return Result{}, fmt.Errorf("queries array is required (1-4 non-empty search queries)")
		}
		if len(a.Queries) > 4 {
			return Result{}, fmt.Errorf("queries accepts at most 4 items")
		}
		answer, hits, err := WebSearch(ctx, a.Queries)
		if err != nil {
			return Result{}, err
		}
		var b strings.Builder
		if answer != "" {
			b.WriteString("answer: " + answer + "\n\n")
		}
		b.WriteString(fmt.Sprintf("sources (%d):\n", len(hits)))
		limit := len(hits)
		if limit > 12 {
			limit = 12
		}
		for i, h := range hits[:limit] {
			fmt.Fprintf(&b, "%d. [%s](%s)", i+1, h.Title, h.URL)
			if h.Snippet != "" {
				fmt.Fprintf(&b, "\n   %s", clipRunes(h.Snippet, 200))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nuse the returned source snippets when available and cite the relevant URLs as markdown links")
		return TextResult(b.String()), nil
	})
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
