package common

import "github.com/gin-gonic/gin"

// BillingSettler 抽象计费会话的生命周期操作。
// 由 service.BillingSession 实现，存储在 RelayInfo 上以避免循环引用。
type BillingSettler interface {
	// Settle 根据实际消耗额度，分别以资金来源预留和 token 预留为基线结算。
	Settle(actualQuota int) error

	// Refund 退还所有预扣费额度（资金来源 + 令牌），幂等安全。
	// 通过 gopool 异步执行。如果已经结算或退款则不做任何操作。
	Refund(c *gin.Context)

	// NeedsRefund 返回会话是否存在需要退还的预扣状态（未结算且未退款）。
	NeedsRefund() bool

	// FundingCommitted 返回资金来源是否已成功提交结算差额。
	// 用于 Settle 失败后的判定：资金差额已提交（仅令牌调整失败）时最终
	// 收费已发生，必须保留消费账目；资金差额未提交时不得记账。
	FundingCommitted() bool

	// GetPreConsumedQuota 返回资金来源的实际预扣额度（信任用户可能为 0）。
	GetPreConsumedQuota() int

	// Reserve 在发送上游前分别将资金和 token 预扣补到目标值；此前命中的
	// 信任旁路不适用于该最终出站载荷预留。已结算或已退款会话返回错误。
	Reserve(targetQuota int) error

	// ReserveStrict 将预扣额度补到目标值，且不允许信任额度旁路。
	// 已结算或已退款的会话会返回错误，资金不足时不得产生部分收费。
	ReserveStrict(targetQuota int) error
}
