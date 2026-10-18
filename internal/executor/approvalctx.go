package executor

import "context"

// "这次调用已经拿到审批"的信号，通过 context 传给执行器。
//
// 为什么用 context 而不是给 Run 加参数：它是"这一次执行的外部许可"，
// 沿着"编排 → 执行器 → 工具"这条调用链往下传最自然；
// 而且它是**一次性的**——不能留在某个结构体上，否则会被下一次调用误用。
type approvalGrantedCtxKey struct{}

// WithApprovalGranted 标记本次执行已获人工批准。
//
// 只有编排层在"工单已批"的分支里能设置它；工具与数据源看不到这层信息。
func WithApprovalGranted(ctx context.Context) context.Context {
	return context.WithValue(ctx, approvalGrantedCtxKey{}, true)
}

// ApprovalGranted 判断本次执行是否已获批。
func ApprovalGranted(ctx context.Context) bool {
	granted, _ := ctx.Value(approvalGrantedCtxKey{}).(bool)
	return granted
}
