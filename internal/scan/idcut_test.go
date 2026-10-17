package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ruanbw/agent-sweep/internal/catalog"
)

// R27：shortID 必须按 rune 截断。这些用例覆盖 ASCII、多字节中文、4 字节
// emoji、ZWJ 组合、混合串，以及空串/非法 UTF-8 的兜底。
func TestShortIDTruncatesASCII(t *testing.T) {
	cases := []struct {
		name string
		id   string
		n    int
		want string
	}{
		{"短串原样返回", "abc-123", 8, "abc-123"},
		{"恰好等于 N 不截断", "abcd", 4, "abcd"},
		{"长串取前 N 个字符", "abcdefghijkl", 8, "abcdefgh"},
		{"N 大于长度时原样返回", "abc", 100, "abc"},
		{"N 为 1", "abcdef", 1, "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shortID(c.id, c.n)
			if got != c.want {
				t.Fatalf("shortID(%q, %d) = %q，期望 %q", c.id, c.n, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("shortID(%q, %d) = %q 不是合法 UTF-8", c.id, c.n, got)
			}
		})
	}
}

func TestShortIDTruncatesChinese(t *testing.T) {
	const id = "会话编号测试一二三四五"
	got := shortID(id, 8)
	const want = "会话编号测试一二"

	if got != want {
		t.Fatalf("shortID(%q, 8) = %q，期望 %q", id, got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("返回 %q 不是合法 UTF-8", got)
	}
	if n := utf8.RuneCountInString(got); n != 8 {
		t.Fatalf("rune 数 = %d，期望 8", n)
	}
	if strings.HasPrefix(got, string(utf8.RuneError)) {
		t.Fatalf("返回 %q 含替换字符，说明切坏了字符", got)
	}
}

func TestShortIDTruncatesEmoji(t *testing.T) {
	t.Run("4 字节 emoji", func(t *testing.T) {
		const id = "😀😃😄😁😆😅😂🤣😊🙂"
		got := shortID(id, 8)
		const want = "😀😃😄😁😆😅😂🤣"

		if got != want {
			t.Fatalf("shortID(%q, 8) = %q，期望 %q", id, got, want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("返回 %q 不是合法 UTF-8", got)
		}
		if n := utf8.RuneCountInString(got); n != 8 {
			t.Fatalf("rune 数 = %d，期望 8", n)
		}
	})

	t.Run("ZWJ 组合 emoji", func(t *testing.T) {
		const family = "👨‍👩‍👧‍👦"
		const id = family + family + family
		got := shortID(id, 8)

		if !utf8.ValidString(got) {
			t.Fatalf("返回 %q 不是合法 UTF-8", got)
		}
		if n := utf8.RuneCountInString(got); n != 8 {
			t.Fatalf("rune 数 = %d，期望 8", n)
		}
		if !strings.HasPrefix(id, got) {
			t.Fatalf("返回 %q 不是原串的 rune 前缀", got)
		}
	})
}

func TestShortIDTruncatesMixed(t *testing.T) {
	const id = "ab中文😀cd日本🎌xy"
	got := shortID(id, 8)
	const want = "ab中文😀cd日"

	if got != want {
		t.Fatalf("shortID(%q, 8) = %q，期望 %q", id, got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("返回 %q 不是合法 UTF-8", got)
	}
	if n := utf8.RuneCountInString(got); n != 8 {
		t.Fatalf("rune 数 = %d，期望 8", n)
	}
}

// 无论 N 取多少（包括 0/负数/超过长度），返回值都必须合法且非空。
func TestShortIDAlwaysValidUTF8(t *testing.T) {
	const id = "a中😀b日🎌c"
	total := utf8.RuneCountInString(id)
	for n := -1; n <= total+2; n++ {
		got := shortID(id, n)
		if got == "" {
			t.Fatalf("shortID(%q, %d) 返回空串", id, n)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("shortID(%q, %d) = %q 不是合法 UTF-8", id, n, got)
		}
		limit := n
		if limit < 1 {
			limit = 1
		}
		if rc := utf8.RuneCountInString(got); rc > limit {
			t.Fatalf("shortID(%q, %d) 的 rune 数 = %d，超过上限 %d", id, n, rc, limit)
		}
	}
}

func TestShortIDEmptyAndInvalidUTF8(t *testing.T) {
	t.Run("空串走兜底", func(t *testing.T) {
		got := shortID("", 8)
		if got == "" {
			t.Fatal("空串不应产出空 key")
		}
		if got != shortIDFallback {
			t.Fatalf("shortID(\"\", 8) = %q，期望兜底值 %q", got, shortIDFallback)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("兜底值 %q 不是合法 UTF-8", got)
		}
	})

	t.Run("全是非法字节", func(t *testing.T) {
		id := string([]byte{0xff, 0xfe, 0x80, 0x81, 0xc0, 0xaf})
		for _, n := range []int{1, 3, 8, 100} {
			got := shortID(id, n)
			if got == "" {
				t.Fatalf("shortID(非法字节, %d) 返回空串", n)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("shortID(非法字节, %d) = %q 不是合法 UTF-8", n, got)
			}
			if rc := utf8.RuneCountInString(got); rc > n {
				t.Fatalf("shortID(非法字节, %d) 的 rune 数 = %d，超过上限", n, rc)
			}
		}
	})

	t.Run("非法字节开头", func(t *testing.T) {
		id := string([]byte{0xff}) + "abc中文会话"
		got := shortID(id, 8)
		if got == "" {
			t.Fatal("非法字节开头不应产出空串")
		}
		if !utf8.ValidString(got) {
			t.Fatalf("shortID = %q 不是合法 UTF-8", got)
		}
		if n := utf8.RuneCountInString(got); n != 8 {
			t.Fatalf("rune 数 = %d，期望 8", n)
		}
	})
}

func TestFallbackTitleUsesRuneSafeCut(t *testing.T) {
	const id = "中文会话😀长名字abc"
	got := fallbackTitle(id)
	want := "会话 " + shortID(id, 8)

	if got != want {
		t.Fatalf("fallbackTitle(%q) = %q，期望 %q", id, got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("fallbackTitle(%q) = %q 不是合法 UTF-8", id, got)
	}
	// fallbackTitle("") 也必须给一个非空、可显示的标题。
	if empty := fallbackTitle(""); empty == "" || !utf8.ValidString(empty) {
		t.Fatalf("fallbackTitle(\"\") = %q，不是合法非空标题", empty)
	}
}

// 回归：真正生成 Session.ID 的调用点产出的 ID 必须是合法 UTF-8，
// 且 UID 与 ID 使用同一个未被截断的值。
func TestBuildFileSessionIDIsValidUTF8(t *testing.T) {
	dir := t.TempDir()
	const name = "中文会话😀日本語テスト.jsonl"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	spec := catalog.Spec{ID: "unicode-test", Name: "Unicode Test"}
	store := catalog.Store{Kind: "jsonl", Glob: "*.jsonl"}
	item := buildFileSession(spec, dir, store, path, info, true)

	wantID := strings.TrimSuffix(name, filepath.Ext(name))
	if item.ID != wantID {
		t.Fatalf("Session.ID = %q，期望完整文件名 %q（ID 不应被截断）", item.ID, wantID)
	}
	if !utf8.ValidString(item.ID) {
		t.Fatalf("Session.ID = %q 不是合法 UTF-8", item.ID)
	}
	if want := spec.ID + ":" + item.ID; item.UID != want {
		t.Fatalf("Session.UID = %q，期望 %q", item.UID, want)
	}
	if !utf8.ValidString(item.UID) {
		t.Fatalf("Session.UID = %q 不是合法 UTF-8", item.UID)
	}
	if item.Title == "" {
		t.Fatal("Session.Title 不应为空")
	}
	if !utf8.ValidString(item.Title) {
		t.Fatalf("Session.Title = %q 不是合法 UTF-8", item.Title)
	}
	if want := "会话 " + shortID(item.ID, 8); item.Title != want {
		t.Fatalf("Session.Title = %q，期望回退标题 %q", item.Title, want)
	}
}
