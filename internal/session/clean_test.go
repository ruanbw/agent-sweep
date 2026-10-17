package session

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// cleanReference 是重构前 clean 的逐字副本。
//
// 保留它做对照：新实现是「单趟按 rune 扫描」，最容易出的错不是 panic，而是
// 边界语义悄悄变了（第 80 字、第 81 个空白、折叠后的尾随空格）。只要两者在
// 表格与随机输入上逐字节相等，重构就只是省掉了两次整串分配。
func cleanReference(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > 80 {
		return strings.TrimSpace(string(runes[:80])) + "…"
	}
	return strings.TrimSpace(s)
}

// TestCleanMatchesReference 覆盖各类输入，额外对关键边界钉死字面结果。
func TestCleanMatchesReference(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		// exact 为 true 时额外断言字面结果；否则只与旧实现对照（emoji 截断
		// 会不会切开 ZWJ 序列这类输入，字面结果不好手写）。
		exact bool
	}{
		{name: "空串", in: "", want: "", exact: true},
		{name: "纯空白", in: " \t\r\n\u00a0\u3000 ", want: "", exact: true},
		{name: "单行", in: "hello world", want: "hello world", exact: true},
		{name: "CRLF 换行", in: "line1\r\nline2", want: "line1 line2", exact: true},
		{name: "连续空白", in: "a  \t\t b", want: "a b", exact: true},
		{name: "前后空白", in: "   padded   ", want: "padded", exact: true},
		{name: "正好 80 个 ASCII", in: strings.Repeat("a", 80), want: strings.Repeat("a", 80), exact: true},
		{name: "81 个 ASCII", in: strings.Repeat("b", 81), want: strings.Repeat("b", 80) + "…", exact: true},
		{name: "中文 81 个", in: strings.Repeat("中", 81), want: strings.Repeat("中", 80) + "…", exact: true},
		{name: "78+空格+1 恰好 80", in: strings.Repeat("c", 78) + " " + "d",
			want: strings.Repeat("c", 78) + " d", exact: true},
		{name: "79+空格+1 截断不留尾空格", in: strings.Repeat("e", 79) + " " + "f",
			want: strings.Repeat("e", 79) + "…", exact: true},
		{name: "80+空格+1 截断不留尾空格", in: strings.Repeat("g", 80) + " " + "h",
			want: strings.Repeat("g", 80) + "…", exact: true},
		{name: "80 字后仅空白不追加省略号", in: strings.Repeat("i", 80) + "   \t\n",
			want: strings.Repeat("i", 80), exact: true},
		{name: "4 字节 emoji", in: strings.Repeat("😀", 81)},
		{name: "ZWJ 家庭 emoji", in: strings.Repeat("👨‍👩‍👧‍👦", 20)},
		{name: "超长正文 200KB", in: strings.Repeat("正文 ", 100_000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clean(tt.in)
			wantRef := cleanReference(tt.in)
			if got != wantRef {
				t.Fatalf("clean 与旧实现不一致\n got: %q\nwant: %q", got, wantRef)
			}
			if tt.exact && got != tt.want {
				t.Fatalf("clean = %q，want %q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("结果不是合法 UTF-8: %q", got)
			}
		})
	}
}

// TestCleanLongInputStaysBounded 断言长正文只产出 81 rune 以内的标题。
func TestCleanLongInputStaysBounded(t *testing.T) {
	body := strings.Repeat("这是一段很长很长的会话正文，包含大量内容。", 10_000)
	got := clean(body)
	if runes := []rune(got); len(runes) > 81 {
		t.Fatalf("标题长 %d rune，超过 80+省略号", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长正文的标题应以省略号结尾: %q", got)
	}
	if clean(body) != cleanReference(body) {
		t.Fatal("长正文下新旧实现不一致")
	}
}

// TestCleanRandomized 用固定种子生成混合字符串，逐字节对照旧实现。
// 固定种子保证失败可复现；不依赖耗时，也不做性能断言。
func TestCleanRandomized(t *testing.T) {
	tokens := []string{
		"a", "Z", "中", "😀", "👨‍👩‍👧‍👦", " ",
		"\t", "\n", "\r", "\r\n", "\u00a0", "\u3000", "  ",
	}
	rng := rand.New(rand.NewSource(20261018))
	for i := 0; i < 2000; i++ {
		var b strings.Builder
		for n := rng.Intn(120); n > 0; n-- {
			b.WriteString(tokens[rng.Intn(len(tokens))])
		}
		input := b.String()
		got := clean(input)
		want := cleanReference(input)
		if got != want {
			t.Fatalf("第 %d 个随机输入不一致\ninput: %q\n got: %q\nwant: %q", i, input, got, want)
		}
	}
}

// BenchmarkClean 只用来观察单条标题的分配；测试不依赖它的耗时数据。
func BenchmarkClean(b *testing.B) {
	body := strings.Repeat("会话正文内容 ", 50_000) // 约 600KB
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = clean(body)
	}
}

// BenchmarkCleanReference 是旧实现的基准，只用于和新实现对照分配次数。
func BenchmarkCleanReference(b *testing.B) {
	body := strings.Repeat("会话正文内容 ", 50_000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = cleanReference(body)
	}
}
