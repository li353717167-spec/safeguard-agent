package tools

import "context"

// 幂等 key 通过 context 传给工具。
//
// 为什么用 context 而不是给每个工具加参数：它是一段**横切信息**（谁在执行、执行的是哪个逻辑操作），
// 每个工具都自己声明一个 idemKey 参数既啰嗦、也容易被漏掉；
// 而 context 天然就是从执行器流到工具的那条路径。工具想用就取，不用就当没看见。
type idemKeyCtxKey struct{}

// WithIdempotencyKey 把幂等 key 放进 ctx（由执行器在调用工具前注入）。
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idemKeyCtxKey{}, key)
}

// IdempotencyKeyFromContext 取出幂等 key。
//
// 写工具必须要求它存在：写操作只有经由幂等执行器发起，"重复执行"才可能被挡住。
func IdempotencyKeyFromContext(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(idemKeyCtxKey{}).(string)
	return key, ok && key != ""
}
