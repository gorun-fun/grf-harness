package js

import (
	"context"
	"strings"
	"testing"
	"time"
)

func runJS(t *testing.T, src string) (Value, error) {
	t.Helper()
	ip := New(nil)
	return ip.RunEval(src)
}

func TestBasicExpr(t *testing.T) {
	v, err := runJS(t, `const x = 1 + 2 * 3; return x;`)
	if err != nil {
		t.Fatal(err)
	}
	if v != 7.0 {
		t.Fatalf("want 7 got %v", v)
	}
}

func TestTemplateAndStr(t *testing.T) {
	v, err := runJS(t, "const n = 3; return `n=${n}, twice=${n * 2}`;")
	if err != nil {
		t.Fatal(err)
	}
	if v != "n=3, twice=6" {
		t.Fatalf("got %q", v)
	}
}

func TestArrowAndMapFilter(t *testing.T) {
	v, err := runJS(t, `
const items = [1, 2, 3, 4];
const out = items.filter(x => x % 2 === 0).map(x => x * 10);
return out.join(",");
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "20,40" {
		t.Fatalf("got %v", v)
	}
}

func TestForOfAndObjects(t *testing.T) {
	v, err := runJS(t, `
const files = ["a.ts", "b.ts"];
let total = 0;
for (const f of files) {
  total += f.length;
}
const obj = { name: "w", n: files.length };
return obj.name + ":" + obj.n + ":" + total;
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "w:2:8" {
		t.Fatalf("got %v", v)
	}
}

func TestTryThrow(t *testing.T) {
	v, err := runJS(t, `
let msg = "";
try {
  throw "boom";
} catch (e) {
  msg = "caught:" + e;
}
return msg;
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "caught:boom" {
		t.Fatalf("got %v", v)
	}
}

func TestAsyncAwaitHost(t *testing.T) {
	host := map[string]HostFn{
		"agent": func(ip *Interp, args []Value) (Value, error) {
			return newFuture(func() (Value, error) {
				time.Sleep(10 * time.Millisecond)
				return "R:" + ToString(Arg(args, 0)), nil
			}), nil
		},
	}
	ip := New(host)
	v, err := ip.RunEval(`
const a = await agent("x");
const b = await agent("y");
return a + "|" + b;
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "R:x|R:y" {
		t.Fatalf("got %v", v)
	}
}

func TestParallelHost(t *testing.T) {
	host := map[string]HostFn{
		"parallel": parallelImpl,
		"agent": func(ip *Interp, args []Value) (Value, error) {
			return newFuture(func() (Value, error) {
				time.Sleep(5 * time.Millisecond)
				return "a" + ToString(Arg(args, 0)), nil
			}), nil
		},
	}
	ip := New(host)
	v, err := ip.RunEval(`
const out = await parallel([1, 2, 3].map(i => () => agent(i)));
return out.join(",");
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "a1,a2,a3" {
		t.Fatalf("got %v", v)
	}
}

// parallelImpl 与 agent 包的 workflow 接线同构的参考实现（供测试）。
func parallelImpl(ip *Interp, args []Value) (Value, error) {
	thunks, ok := Arg(args, 0).([]any)
	if !ok {
		return nil, rtErr("parallel expects an array of thunks")
	}
	results := make([]any, len(thunks))
	done := make(chan int, len(thunks))
	for i, th := range thunks {
		i, th := i, th
		go func() {
			v, err := ip.CallValue(th)
			if err != nil {
				results[i] = nil
			} else {
				results[i] = v
			}
			done <- i
		}()
	}
	for range thunks {
		<-done
	}
	return results, nil
}

func TestPipelineHost(t *testing.T) {
	host := map[string]HostFn{
		"pipeline": func(ip *Interp, args []Value) (Value, error) {
			items, _ := Arg(args, 0).([]any)
			stages := make([]*Fn, 0)
			for _, s := range args[1:] {
				if f, ok := s.(*Fn); ok {
					stages = append(stages, f)
				}
			}
			results := make([]any, len(items))
			done := make(chan int, len(items))
			for idx, it := range items {
				idx, it := idx, it
				go func() {
					prev := it
					for _, st := range stages {
						v, err := ip.CallValue(st, prev, it, float64(idx))
						if err != nil {
							prev = nil
							break
						}
						prev = v
					}
					results[idx] = prev
					done <- idx
				}()
			}
			for range items {
				<-done
			}
			return results, nil
		},
	}
	ip := New(host)
	v, err := ip.RunEval(`
const out = await pipeline([1, 2], (prev, item) => prev * 100, (prev, item, i) => prev + i);
return out.join(",");
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "100,201" {
		t.Fatalf("got %v", v)
	}
}

func TestOptionalChainAndNullish(t *testing.T) {
	v, err := runJS(t, `const o = {}; return (o?.missing?.deep ?? "none");`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "none" {
		t.Fatalf("got %v", v)
	}
}

func TestStringMethods(t *testing.T) {
	v, err := runJS(t, `
const s = "hello world";
return s.toUpperCase() + "|" + s.split(" ").join("_") + "|" + s.includes("world") + "|" + s.slice(0, 5);
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "HELLO WORLD|hello_world|true|hello" {
		t.Fatalf("got %v", v)
	}
}

func TestWhileAndAssign(t *testing.T) {
	v, err := runJS(t, `
let i = 0;
let acc = "";
while (i < 3) {
  i += 1;
  acc += i;
}
return acc;
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "123" {
		t.Fatalf("got %v", v)
	}
}

func TestJSONBuiltin(t *testing.T) {
	v, err := runJS(t, `
const o = JSON.parse('{"a": 1, "b": [true, null]}');
return JSON.stringify(o);
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != `{"a":1,"b":[true,null]}` {
		t.Fatalf("got %v", v)
	}
}

func TestFunctionClosure(t *testing.T) {
	v, err := runJS(t, `
function make(n) {
  return (x) => x + n;
}
const add5 = make(5);
return add5(10);
`)
	if err != nil {
		t.Fatal(err)
	}
	if v != 15.0 {
		t.Fatalf("got %v", v)
	}
}

func TestHostMisuseKillsScript(t *testing.T) {
	host := map[string]HostFn{
		"agent": func(ip *Interp, args []Value) (Value, error) {
			return nil, rtErr("agent expects a string prompt")
		},
	}
	ip := New(host)
	_, err := ip.RunEval(`await agent(123);`)
	if err == nil || !strings.Contains(err.Error(), "expects a string prompt") {
		t.Fatalf("want propagated error, got %v", err)
	}
}

func TestCondExpr(t *testing.T) {
	v, err := runJS(t, `const n = 5; return n > 3 ? "big" : "small";`)
	if err != nil {
		t.Fatal(err)
	}
	if v != "big" {
		t.Fatalf("got %v", v)
	}
}

// ---- 资源护栏回归（安全修复：失控递归/自旋曾可杀死进程或永久卡死回合）----

func TestGuardRunawayRecursion(t *testing.T) {
	ip := New(nil)
	// 26 字节即可触发未防护时的 Go 栈溢出 fatal（不可 recover）
	_, err := ip.RunEval(`const f = (n) => f(n+1); return f(0);`)
	if err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("want depth-limit error, got %v", err)
	}
}

func TestGuardInfiniteLoopStepBudget(t *testing.T) {
	ip := New(nil)
	_, err := ip.RunEval(`var i = 0; while (true) { i++; } return i;`)
	if err == nil || !strings.Contains(err.Error(), "step budget") {
		t.Fatalf("want step-budget error, got %v", err)
	}
}

func TestGuardLoopCtxCancel(t *testing.T) {
	ip := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := ip.RunEvalCtx(ctx, `var i = 0; while (true) { i++; } return i;`)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("want cancellation error, got %v", err)
	}
}

func TestGuardParseDepth(t *testing.T) {
	// 深嵌套括号：未防护时解析器自身递归栈溢出（fatal）
	_, err := Parse(strings.Repeat("(", 200_000) + "1" + strings.Repeat(")", 200_000))
	if err == nil || !strings.Contains(err.Error(), "nesting too deep") {
		t.Fatalf("want parse-depth error, got %v", err)
	}
}

func TestGuardStringBombs(t *testing.T) {
	ip := New(nil)
	if _, err := ip.RunEval(`return "x".repeat(-1);`); err == nil {
		t.Fatal("negative repeat must error")
	}
	if _, err := ip.RunEval(`return "x".repeat(1000000000);`); err == nil {
		t.Fatal("huge repeat must error")
	}
	if _, err := ip.RunEval(`var s = "a"; for (var i = 0; i < 40; i++) { s = s + s; } return s;`); err == nil {
		t.Fatal("doubling concat must error past cap")
	}
}
