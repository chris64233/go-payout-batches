package gopayoutbatches

import "errors"

// 领域错误全部以哨兵错误（sentinel errors）形式定义，
// 调用方可用 errors.Is(err, ErrXxx) 精确区分错误类型。
var (
	// ErrNotFound 请求的资源（应付款 / 批次 / 提交记录）不存在。
	ErrNotFound = errors.New("resource not found")

	// ErrInvalidArgument 入参不合法（空字段、金额非正、重复明细等）。
	ErrInvalidArgument = errors.New("invalid argument")

	// ErrConflict 与当前状态冲突，但不属于下面更具体的情况
	// （例如批次总金额与提交内容不一致）。
	ErrConflict = errors.New("conflict")

	// ErrForbidden 资源存在但不属于当前商户。
	ErrForbidden = errors.New("forbidden")

	// ErrItemNotSettlable 明细不在“可结算”状态，不能进入批次。
	ErrItemNotSettlable = errors.New("payable item is not settlable")

	// ErrAlreadySubmitted 批次已经提交，不能再执行本地取消。
	ErrAlreadySubmitted = errors.New("batch already submitted")

	// ErrBatchClosed 批次已结束（成功 / 失败 / 已取消），不可再变更。
	ErrBatchClosed = errors.New("batch already closed")

	// ErrUnknownSubmission 银行回执引用了系统中不存在的
	// （外部提交号, 提交版本）组合，通常是乱序或迟到的回执。
	ErrUnknownSubmission = errors.New("unknown bank submission")

	// ErrReceiptMismatch 回执内容与当前提交的批次 / 金额不一致。
	ErrReceiptMismatch = errors.New("bank receipt does not match submission")

	// ErrStaleReceipt 回执属于旧提交版本，批次已有更新的提交，旧回执无效。
	ErrStaleReceipt = errors.New("stale bank receipt for previous submission version")

	// ErrTerminalState 批次已处于成功 / 失败终态，终态不可被任何回执覆盖。
	ErrTerminalState = errors.New("batch already in terminal state")

	// ErrDuplicateRequest 幂等键重复但请求内容不同。
	ErrDuplicateRequest = errors.New("duplicate idempotency key with different payload")
)
