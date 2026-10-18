package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// maxCanonicalDepth 限制嵌套深度。
// 参数来自模型，必须假设它可能被构造成深嵌套来打爆递归——这里是 fail closed 的地方。
const maxCanonicalDepth = 32

// CanonicalJSON 把 JSON 重写成**确定性**的规范形式，用于生成幂等 key。
//
// 三条规则（多一条都不需要）：
//  1. object 的键按字节序排序 —— Go 里 map 的遍历顺序是随机的，不排序就永远算不出稳定的 key；
//  2. 数字用**精确十进制**规范化 —— "1" / "1.0" / "1e0" / "0.1e1" 必须都变成 "1"，
//     否则同一次操作会因为参数写法不同而算出不同的 key，幂等直接失效；
//  3. 数组保持原顺序 —— 数组有序是语义的一部分（[a,b] 与 [b,a] 不是同一个操作）。
//
// 数字用 math/big.Rat 而不是 float64，是为了**大整数不丢精度**：
// float64 在 2^53 之后开始舍入，而"舍入后相同"意味着两个不同的操作可能算出同一个 key——
// 那是幂等系统里最坏的错误方向（该执行的不执行）。
func CanonicalJSON(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留原始数字字面量，交给 big.Rat 精确解析
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("executor: 参数不是合法 JSON: %w", err)
	}

	var buf strings.Builder
	if err := writeCanonical(&buf, v, 0); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeCanonical(buf *strings.Builder, v any, depth int) error {
	if depth > maxCanonicalDepth {
		return fmt.Errorf("executor: 参数嵌套超过 %d 层，拒绝处理", maxCanonicalDepth)
	}

	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("executor: 序列化字符串失败: %w", err)
		}
		buf.Write(encoded)
	case json.Number:
		num, err := canonicalNumber(string(val))
		if err != nil {
			return err
		}
		buf.WriteString(num)
	case []any:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item, depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("executor: 序列化键失败: %w", err)
			}
			buf.Write(encoded)
			buf.WriteByte(':')
			if err := writeCanonical(buf, val[k], depth+1); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("executor: 不支持的类型 %T", v)
	}
	return nil
}

// canonicalNumber 把数字字面量规范化。
//
//	"1" "1.0" "1e0" "0.1e1"  → "1"
//	"0.10"                    → "0.1"
//	"12345678901234567890"    → 原样（不丢精度）
func canonicalNumber(literal string) (string, error) {
	r, ok := new(big.Rat).SetString(literal)
	if !ok {
		return "", fmt.Errorf("executor: 无法解析数字 %q", literal)
	}
	if r.IsInt() {
		return r.Num().String(), nil
	}
	// 非整数：取 20 位小数再去尾零。
	// 20 位足够覆盖工具参数（阈值、百分比、窗口），同时保证确定性。
	s := r.FloatString(20)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		// 极小的非零值：保留原始字面量，好过静默变成 0。
		return literal, nil
	}
	return s, nil
}
