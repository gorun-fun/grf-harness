// snapshot.go 页面快照与 ref 引用机制：快照时给可交互元素打上
// data-grf-ref 属性并输出带缩进的大纲文本（角色 + 名称 + [ref=eN]），
// 后续 click/type/hover/select 以 ref 定位——与 playwright-mcp 的
// snapshot→ref→操作工作流对齐。遍历与定位均穿透 shadow DOM 与同源
// iframe（跨域 iframe 无法注入，标记后跳过）。页面导航后属性失效，
// 工具会提示重拍。
package main

import (
	"encoding/json"
	"fmt"

	"github.com/chromedp/chromedp"
)

// deepQueryJS 定义 window.__grfDeepQuery：按 CSS 选择器深度查找元素
// （document → shadow root → 同源 iframe 文档，递归）。快照时定义，
// 后续 click/type 等操作的表达式里以 dq() 引用——导航会清掉它，但那种
// 场景 ref 本来就失效了，dq 会退化到 document.querySelector 兜底。
const deepQueryJS = `window.__grfDeepQuery = window.__grfDeepQuery || ((sel) => {
  const search = (root) => {
    const hit = root.querySelector(sel);
    if (hit) return hit;
    for (const el of root.querySelectorAll('*')) {
      if (el.shadowRoot) {
        const r = search(el.shadowRoot);
        if (r) return r;
      }
    }
    for (const f of root.querySelectorAll('iframe')) {
      try {
        if (f.contentDocument) {
          const r = search(f.contentDocument);
          if (r) return r;
        }
      } catch (e) {}
    }
    return null;
  };
  return search(document);
});`

// dqHelper 操作表达式里取深度查找函数（未定义则退化为仅主文档）。
const dqHelper = `const dq = window.__grfDeepQuery || (s => document.querySelector(s));`

// snapshotJS DOM 大快照：返回大纲文本。交互元素打 data-grf-ref，
// 纯文本块（p/li/td 等）以 text 行输出，遍历穿透 shadow DOM 与同源
// iframe。
const snapshotJS = `(() => {
  ` + deepQueryJS + `
  document.querySelectorAll('[data-grf-ref]').forEach(el => el.removeAttribute('data-grf-ref'));
  const sweep = (root) => {
    root.querySelectorAll('*').forEach(el => {
      el.removeAttribute('data-grf-ref');
      if (el.shadowRoot) sweep(el.shadowRoot);
    });
    for (const f of root.querySelectorAll('iframe')) {
      try { if (f.contentDocument) sweep(f.contentDocument); } catch (e) {}
    }
  };
  sweep(document);
  const root = document.body || document.documentElement;
  if (!root) return '(空页面)';
  const lines = [];
  let n = 0, overflow = false;
  const MAXLINES = 800;
  const INTERACTIVE_TAGS = new Set(['a','button','input','select','textarea','summary','label']);
  const LANDMARKS = new Set(['main','nav','header','footer','aside','form','table','article','section']);
  const HEADINGS = new Set(['h1','h2','h3','h4','h5','h6']);
  const TEXT_TAGS = new Set(['p','li','td','th','blockquote','pre','dd','dt','figcaption','code']);
  const ROLE_TAGS = new Set(['button','link','checkbox','radio','tab','menuitem','option','combobox','textbox','searchbox','switch','slider','treeitem','tablist','dialog']);
  const hidden = (el) => {
    const cs = getComputedStyle(el);
    return cs.display === 'none' || cs.visibility === 'hidden';
  };
  const roleOf = (el) => {
    const r = el.getAttribute('role');
    if (r) return r;
    const tag = el.tagName.toLowerCase();
    if (tag === 'a' && el.hasAttribute('href')) return 'link';
    if (tag === 'button') return 'button';
    if (tag === 'img') return 'image';
    if (tag === 'select') return 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'input') {
      const t = (el.getAttribute('type') || 'text').toLowerCase();
      if (t === 'checkbox') return 'checkbox';
      if (t === 'radio') return 'radio';
      if (t === 'submit' || t === 'button' || t === 'file' || t === 'reset') return 'button';
      return 'textbox';
    }
    if (tag === 'summary') return 'button';
    if (HEADINGS.has(tag)) return 'heading';
    return tag;
  };
  const nameOf = (el) => {
    let s = el.getAttribute('aria-label') || el.getAttribute('alt') || el.getAttribute('placeholder') || el.getAttribute('title') || '';
    const tag = el.tagName.toLowerCase();
    if (!s && (tag === 'input' || tag === 'textarea')) s = el.value || '';
    if (!s) s = (el.innerText || '').replace(/\s+/g, ' ').trim();
    if (!s) s = el.getAttribute('value') || '';
    s = s.replace(/\s+/g, ' ').trim();
    if (s.length > 100) s = s.slice(0, 100) + '…';
    return s;
  };
  const isInteractive = (el) => {
    if (el.hasAttribute('disabled')) return false;
    const tag = el.tagName.toLowerCase();
    if (INTERACTIVE_TAGS.has(tag)) return true;
    if (el.hasAttribute('onclick') || el.isContentEditable) return true;
    return ROLE_TAGS.has(el.getAttribute('role') || '');
  };
  // ownText 只取元素直接持有的文本节点（子块级元素的文本由它们自己输出）
  const ownText = (el) => {
    let s = '';
    for (const n of el.childNodes) {
      if (n.nodeType === Node.TEXT_NODE) s += n.textContent;
    }
    return s.replace(/\s+/g, ' ').trim();
  };
  const markRef = (el) => {
    const ref = 'e' + (++n);
    el.setAttribute('data-grf-ref', ref);
    return ref;
  };
  const walk = (el, depth) => {
    if (overflow || lines.length >= MAXLINES) { overflow = true; return; }
    const tag = el.tagName.toLowerCase();
    if (tag === 'script' || tag === 'style' || tag === 'noscript' || tag === 'template' || tag === 'svg') return;
    if (el.getAttribute('aria-hidden') === 'true') return;
    if (hidden(el)) return;
    const interactive = isInteractive(el);
    const interesting = interactive || LANDMARKS.has(tag) || HEADINGS.has(tag) || tag === 'img' || TEXT_TAGS.has(tag);
    if (!interesting) {
      descend(el, depth);
      return;
    }
    const role = roleOf(el);
    let line = '  '.repeat(depth) + '- ' + role;
    const nm = nameOf(el);
    if (nm) line += ' "' + nm.replace(/"/g, "'") + '"';
    if (interactive) line += ' [ref=' + markRef(el) + ']';
    lines.push(line);
    // 纯文本块自身文字就是名称（上面已输出），不再重复输出 text 行
    if (tag !== 'select' && tag !== 'input' && tag !== 'textarea' && depth < 18) {
      descend(el, depth + 1);
    }
  };
  // descend 遍历子节点：普通子元素 + shadow root + 同源 iframe 文档
  const descend = (el, depth) => {
    if (el.tagName && el.tagName.toLowerCase() === 'iframe') {
      try {
        const doc = el.contentDocument;
        if (doc && doc.body) {
          for (const c of doc.body.children) walk(c, depth);
        } else {
          lines.push('  '.repeat(depth) + '- iframe "(跨域或未加载，不可注入)"');
        }
      } catch (e) {
        lines.push('  '.repeat(depth) + '- iframe "(跨域，不可注入)"');
      }
      return;
    }
    for (const c of el.children) walk(c, depth);
    if (el.shadowRoot) {
      for (const c of el.shadowRoot.children) walk(c, depth);
    }
  };
  walk(root, 0);
  if (overflow) lines.push('…（内容超出快照上限，可用 browser_evaluate 精查）');
  return lines.join('\n');
})()`

// runEval 在活跃标签页求值 JS 表达式（EvaluateAsDevTools：页面异常以
// Go error 透出），JSON 结果解到 out（传指针）。
func (b *browser) runEval(expr string, out any) error {
	return chromedp.Run(b.activeTab().ctx, chromedp.EvaluateAsDevTools(expr, out))
}

// refSelector ref → CSS 选择器（快照标注的属性选择器）。
func refSelector(ref string) string { return `[data-grf-ref="` + ref + `"]` }

// deepCenter 深度查找选择器指向的元素并返回**主视口**中心坐标（真实鼠标
// 事件用；元素在 iframe 内时逐级累加外层 iframe 的偏移，rect 本身相对
// 所属 iframe 视口）。找不到返回错误。
func (b *browser) deepCenter(sel string) (x, y float64, err error) {
	var box map[string]any
	expr := fmt.Sprintf(`(() => {
		%s
		const el = dq(%s);
		if (!el) return null;
		const r = el.getBoundingClientRect();
		let x = r.left + r.width / 2, y = r.top + r.height / 2;
		let doc = el.ownerDocument;
		while (doc && doc !== document) {
			const fe = doc.defaultView && doc.defaultView.frameElement;
			if (!fe) break;
			const fr = fe.getBoundingClientRect();
			x += fr.left; y += fr.top;
			doc = fe.ownerDocument;
		}
		return {x, y};
	})()`, dqHelper, jsString(sel))
	if err := b.runEval(expr, &box); err != nil {
		return 0, 0, err
	}
	if box == nil {
		return 0, 0, fmt.Errorf("元素不存在: %s（页面已变化？请重新 browser_snapshot）", sel)
	}
	x, _ = box["x"].(float64)
	y, _ = box["y"].(float64)
	return x, y, nil
}

// jsString 把 Go 字符串安全地编码为 JS 字符串字面量。
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
