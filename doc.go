// Package gopayoutbatches 实现商户应付款的批次冻结与银行结果确认：
// 应付款登记、批次创建（整批冻结 + 金额快照）、幂等提交、提交前取消、
// 银行回执确认（唯一结算记录与通知）以及批次明细查询。
package gopayoutbatches
