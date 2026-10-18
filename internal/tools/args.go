package tools

import (
	"bytes"
	"encoding/json"
	"io"
)

// decodeArgs 把模型给的原始参数 JSON 严格解码成强类型结构。
//
// 三条规矩，每条都对应一类真实故障：
//  1. 必须是 JSON 对象。模型偶尔会返回数组或裸字符串（尤其在被诱导时）。
//  2. **未知字段直接拒绝**（DisallowUnknownFields）。模型的"幻觉参数"必须在进入业务逻辑前
//     被挡住。静默忽略更糟：模型会以为参数生效了，于是拿同一个错参数反复重试。
//  3. 只允许一个 JSON 值。`{}{}` 这种尾巴不能被静默丢掉。
func decodeArgs[A Args](raw json.RawMessage) (A, error) {
	var out A

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		// 无参数工具的合法写法；有必填字段的工具会在 Validate 里被拦下。
		trimmed = []byte("{}")
	}
	if trimmed[0] != '{' {
		return out, argErrf("参数必须是 JSON 对象，实际是 %s", jsonKind(trimmed))
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, argErrf("解码失败: %v（未知字段与类型不符都会被拒绝，不会静默忽略）", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return out, argErrf("参数后存在多余内容")
	}
	return out, nil
}

func jsonKind(raw []byte) string {
	if len(raw) == 0 {
		return "空"
	}
	switch raw[0] {
	case '[':
		return "数组"
	case '"':
		return "字符串"
	case 't', 'f':
		return "布尔值"
	case 'n':
		return "null"
	default:
		return "数字"
	}
}
