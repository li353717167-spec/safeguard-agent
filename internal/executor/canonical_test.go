package executor

import (
	"strings"
	"testing"
)

func TestCanonicalJSONSortsObjectKeys(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"键顺序不影响结果", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"嵌套对象同样排序", `{"z":{"y":1,"x":2},"a":[{"d":4,"c":3}]}`, `{"a":[{"c":3,"d":4}],"z":{"x":2,"y":1}}`},
		{"数组内对象也排序", `[{"b":1,"a":2}]`, `[{"a":2,"b":1}]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("CanonicalJSON 失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  = %s\nwant = %s", got, tc.want)
			}
		})
	}
}

// 这条是整个幂等体系的地基：同一个操作，参数写法不同也必须得到同一个 key。
func TestCanonicalJSONNormalizesNumbers(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"整数", `{"x":1}`, `{"x":1}`},
		{"带小数点", `{"x":1.0}`, `{"x":1}`},
		{"科学计数", `{"x":1e0}`, `{"x":1}`},
		{"小数移位", `{"x":0.1e1}`, `{"x":1}`},
		{"尾零", `{"x":0.10}`, `{"x":0.1}`},
		{"负数", `{"x":-2.50}`, `{"x":-2.5}`},
		{"大整数不丢精度", `{"x":12345678901234567890}`, `{"x":12345678901234567890}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON([]byte(tc.in))
			if err != nil {
				t.Fatalf("CanonicalJSON 失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  = %s\nwant = %s", got, tc.want)
			}
		})
	}
}

// 数组有序是语义的一部分：[a,b] 与 [b,a] 不是同一个操作。
func TestCanonicalJSONKeepsArrayOrder(t *testing.T) {
	a, err := CanonicalJSON([]byte(`[1,2]`))
	if err != nil {
		t.Fatalf("CanonicalJSON 失败: %v", err)
	}
	b, err := CanonicalJSON([]byte(`[2,1]`))
	if err != nil {
		t.Fatalf("CanonicalJSON 失败: %v", err)
	}
	if a == b {
		t.Errorf("数组顺序被抹平了：%s", a)
	}
}

func TestCanonicalJSONScalars(t *testing.T) {
	cases := []struct{ in, want string }{
		{`null`, `null`},
		{`true`, `true`},
		{`false`, `false`},
		{`"hello"`, `"hello"`},
		{`{"s":"中文"}`, `{"s":"中文"}`},
	}
	for _, tc := range cases {
		got, err := CanonicalJSON([]byte(tc.in))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s) 失败: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("CanonicalJSON(%s) = %s, 期望 %s", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalJSONRejectsBadInput(t *testing.T) {
	if _, err := CanonicalJSON([]byte(`{`)); err == nil {
		t.Error("非法 JSON 应当报错")
	}
	if _, err := CanonicalJSON([]byte(``)); err == nil {
		t.Error("空输入应当报错")
	}

	// 深嵌套必须被拒绝：参数来自模型，不能假设它是善意的。
	deep := strings.Repeat(`{"a":`, maxCanonicalDepth+2) + `1` + strings.Repeat(`}`, maxCanonicalDepth+2)
	if _, err := CanonicalJSON([]byte(deep)); err == nil {
		t.Error("超过深度上限应当报错")
	}
}
