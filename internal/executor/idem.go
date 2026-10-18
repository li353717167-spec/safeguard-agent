package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	// idemKeyVersion 参与哈希：将来若改变规范化规则，新旧 key 不会撞在一起。
	idemKeyVersion = "v1"
	// idemKeyLen 是截断后的十六进制长度（32 个字符 = 128 bit）。
	// 与 0001_init.sql 里 tool_calls.idem_key CHAR(32) 对齐。
	idemKeyLen = 32
)

// BuildIdempotencyKey 生成幂等 key。
//
// 组成：版本 + task_id + step_no + call_index + tool_name + 规范化参数。
// 这五项一起定义了"是不是同一个逻辑操作"：
//   - task_id / step_no / call_index：不同位置的操作不会互相覆盖；
//   - tool_name：相同参数调不同工具不会撞；
//   - 规范化参数：参数写法不同但语义相同，算同一个操作。
//
// ⚠️ 它**不包含**时间、随机数、模型给的工具调用 ID（`call_xxx`）——那些每次都可能不同，
// 混进来之后"同一个操作"就永远算不出同一个 key，幂等 key 也就不是幂等 key 了。
func BuildIdempotencyKey(taskID string, stepNo, callIndex int, toolName string, rawArgs json.RawMessage) (string, error) {
	canonical, err := CanonicalJSON(rawArgs)
	if err != nil {
		return "", fmt.Errorf("executor: 生成幂等 key 失败: %w", err)
	}

	h := sha256.New()
	// 每个字段带长度前缀，避免 "ab"+"c" 与 "a"+"bc" 拼出同一个串。
	writeField := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte(":"))
		h.Write([]byte(s))
		h.Write([]byte("|"))
	}
	writeField(idemKeyVersion)
	writeField(taskID)
	writeField(strconv.Itoa(stepNo))
	writeField(strconv.Itoa(callIndex))
	writeField(toolName)
	writeField(canonical)

	return hex.EncodeToString(h.Sum(nil))[:idemKeyLen], nil
}
