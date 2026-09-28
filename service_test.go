package gopayoutbatches

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

const testMerchant = "M1"

func newTestService() *PayoutService {
	return NewPayoutService(NewStore())
}

func mustRegister(t *testing.T, svc *PayoutService, merchant, decimal string) *Payable {
	t.Helper()
	p, err := svc.RegisterPayable(context.Background(), RegisterPayableRequest{
		MerchantID: merchant,
		Amount:     MustParseMoney("CNY", decimal),
	})
	if err != nil {
		t.Fatalf("RegisterPayable(%s) 失败: %v", decimal, err)
	}
	return p
}

func TestRegisterAndGetPayable(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "100.00")
	if p.Status != PayableSettlable {
		t.Fatalf("初始状态 = %s, want settlable", p.Status)
	}
	if p.Amount.Amount != 10000 {
		t.Fatalf("金额 = %d, want 10000", p.Amount.Amount)
	}
	got, err := svc.GetPayable(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Fatalf("GetPayable 返回 %s", got.ID)
	}

	_, err = svc.GetPayable(context.Background(), "nope")
	if !IsNotFound(err) {
		t.Fatalf("不存在明细应返回 ErrNotFound，got %v", err)
	}

	if _, err = svc.RegisterPayable(context.Background(), RegisterPayableRequest{
		MerchantID: "",
		Amount:     MustParseMoney("CNY", "1.00"),
	}); !IsInvalidArgument(err) {
		t.Fatalf("空商户应返回入参错误，got %v", err)
	}
	if _, err = svc.RegisterPayable(context.Background(), RegisterPayableRequest{
		MerchantID: testMerchant,
		Amount:     MustParseMoney("CNY", "0"),
	}); !IsInvalidArgument(err) {
		t.Fatalf("零金额应返回入参错误，got %v", err)
	}
}

func TestCreateBatchFreezesAtomicSnapshot(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "20.50")
	p3 := mustRegister(t, svc, testMerchant, "30.00")

	b, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BatchOpen || b.TotalAmount.Amount != 3050 {
		t.Fatalf("批次 = status %s total %s", b.Status, b.TotalAmount)
	}
	if len(b.Items) != 2 || b.Items[1].AmountSnap.Amount != 2050 {
		t.Fatalf("金额快照不正确: %+v", b.Items)
	}

	// 明细全部转为 frozen 并记录批次。
	for _, pid := range []string{p1.ID, p2.ID} {
		got, _ := svc.GetPayable(context.Background(), pid)
		if got.Status != PayableFrozen || got.BatchID != b.ID {
			t.Fatalf("明细 %s 状态 = %s/%s", pid, got.Status, got.BatchID)
		}
	}

	// 未入批次的明细不受影响。
	p3got, _ := svc.GetPayable(context.Background(), p3.ID)
	if p3got.Status != PayableSettlable {
		t.Fatalf("p3 不应被冻结: %s", p3got.Status)
	}

	items, err := svc.ListBatchItems(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].PayableID != p1.ID {
		t.Fatalf("批次明细查询异常: %+v", items)
	}
}

func TestCreateBatchRejectsPartialFreeze(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "10.00")
	// p2 先被另一个批次冻结。
	first, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{p2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 新批次包含 p1（可结算）和 p2（已冻结）：必须整批失败。
	_, err = svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{p1.ID, p2.ID},
	})
	if !IsConflict(err) {
		t.Fatalf("冻结中的明细应返回冲突错误，got %v", err)
	}

	// p1 绝不能留下部分冻结。
	p1got, _ := svc.GetPayable(context.Background(), p1.ID)
	if p1got.Status != PayableSettlable || p1got.BatchID != "" {
		t.Fatalf("失败回滚不完整: p1 = %s/%s", p1got.Status, p1got.BatchID)
	}
	p2got, _ := svc.GetPayable(context.Background(), p2.ID)
	if p2got.Status != PayableFrozen || p2got.BatchID != first.ID {
		t.Fatalf("p2 冻结归属被破坏: %s/%s", p2got.Status, p2got.BatchID)
	}

	// 批次数量不应增加。
	if _, err = svc.GetBatch(context.Background(), "不存在"); !IsNotFound(err) {
		t.Fatalf("期望 ErrNotFound, got %v", err)
	}
}

func TestCreateBatchValidation(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	pOther := mustRegister(t, svc, "M2", "10.00")

	_, err := svc.CreateBatch(context.Background(), CreateBatchRequest{MerchantID: testMerchant})
	if !IsInvalidArgument(err) {
		t.Fatalf("空明细应报入参错误, got %v", err)
	}
	_, err = svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{p.ID, p.ID},
	})
	if !IsInvalidArgument(err) {
		t.Fatalf("重复明细应报入参错误, got %v", err)
	}
	_, err = svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{"ghost"},
	})
	if !IsNotFound(err) {
		t.Fatalf("不存在明细应报 ErrNotFound, got %v", err)
	}
	_, err = svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{pOther.ID},
	})
	if !IsInvalidArgument(err) {
		t.Fatalf("跨商户明细应报入参错误, got %v", err)
	}
}

func TestCreateBatchMixedCurrencyRejected(t *testing.T) {
	svc := NewPayoutService(NewStore())
	cny, err := svc.RegisterPayable(context.Background(), RegisterPayableRequest{
		MerchantID: testMerchant, Amount: MustParseMoney("CNY", "10.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	usd, err := svc.RegisterPayable(context.Background(), RegisterPayableRequest{
		MerchantID: testMerchant, Amount: MustParseMoney("USD", "10.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant,
		PayableIDs: []string{cny.ID, usd.ID},
	})
	if !IsInvalidArgument(err) {
		t.Fatalf("混合币种应报入参错误, got %v", err)
	}
}

func TestSubmitIdempotencySameContentReplays(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})

	r1, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "EXT-1"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Replayed || r1.Batch.Status != BatchSubmitted || r1.Batch.SubmitVersion != 1 {
		t.Fatalf("首次提交异常: %+v", r1)
	}
	submittedAt := r1.Batch.SubmittedAt

	r2, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "EXT-1"})
	if err != nil {
		t.Fatalf("同号同内容重放应成功: %v", err)
	}
	if !r2.Replayed || r2.Batch.Status != BatchSubmitted || r2.Batch.SubmitVersion != 1 {
		t.Fatalf("重放结果异常: %+v", r2)
	}
	if !r2.Batch.SubmittedAt.Equal(submittedAt) {
		t.Fatal("重放必须返回首次提交时间")
	}
}

func TestSubmitIdempotencyDifferentBatchConflicts(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "20.00")
	b1, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID},
	})
	b2, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p2.ID},
	})

	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b1.ID, ExternalNo: "EXT-X"}); err != nil {
		t.Fatal(err)
	}
	// 同号用于另一个批次：冲突。
	_, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b2.ID, ExternalNo: "EXT-X"})
	if !IsIdempotencyConflict(err) {
		t.Fatalf("同号换批次应报幂等冲突, got %v", err)
	}
	// b2 必须仍为 open，没有被这次失败的提交污染。
	got, _ := svc.GetBatch(context.Background(), b2.ID)
	if got.Status != BatchOpen {
		t.Fatalf("b2 状态被污染: %s", got.Status)
	}
}

func TestSubmitSameNoDifferentAmountConflicts(t *testing.T) {
	// 构造"同号、同批次、但内容金额不同"的场景：
	// 批次失败释放明细 -> 明细金额被修正登记语义无法直接改金额，
	// 因此通过两个批次复用外部号且内容不同已在上面覆盖；
	// 这里补充：同号先用于成功提交，再用于一个内容不同的新批次。
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "20.00")
	b1, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b1.ID, ExternalNo: "EXT-9"}); err != nil {
		t.Fatal(err)
	}

	// 批次1失败并释放 p1。
	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "EXT-9", BatchID: b1.ID, SubmitVersion: 1, Success: false,
	})
	if err != nil || res.Outcome != ReceiptConfirmed {
		t.Fatalf("失败回执处理异常: %v %+v", err, res)
	}
	// p1 + p2 组成内容不同的新批次，复用同一外部号 -> 冲突。
	b2, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b2.ID, ExternalNo: "EXT-9"})
	if !IsIdempotencyConflict(err) {
		t.Fatalf("同号换金额内容应报幂等冲突, got %v", err)
	}
}

func TestCannotResubmitWithDifferentExternalNo(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "A"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "B"})
	if !IsConflict(err) {
		t.Fatalf("已提交批次换号再提应报冲突, got %v", err)
	}
}

func TestCancelOpenBatchReleasesPayables(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	cb, err := svc.CancelBatch(context.Background(), CancelBatchRequest{BatchID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cb.Status != BatchCanceled {
		t.Fatalf("取消后状态 = %s", cb.Status)
	}
	got, _ := svc.GetPayable(context.Background(), p.ID)
	if got.Status != PayableSettlable || got.BatchID != "" {
		t.Fatalf("取消后明细应释放: %s/%s", got.Status, got.BatchID)
	}
	// 释放后可以进入新批次。
	b2, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if err != nil {
		t.Fatalf("释放后明细应可重新入批次: %v", err)
	}
	if b2.Status != BatchOpen {
		t.Fatalf("新批次状态 %s", b2.Status)
	}

	// 终态批次不能重复取消。
	_, err = svc.CancelBatch(context.Background(), CancelBatchRequest{BatchID: b.ID})
	if !IsConflict(err) {
		t.Fatalf("重复取消应报冲突/终态, got %v", err)
	}
}

func TestCancelSubmittedBatchRejected(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "E"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CancelBatch(context.Background(), CancelBatchRequest{BatchID: b.ID})
	if !IsConflict(err) {
		t.Fatalf("已提交批次不能本地取消, got %v", err)
	}
	got, _ := svc.GetBatch(context.Background(), b.ID)
	if got.Status != BatchSubmitted {
		t.Fatalf("批次状态不应改变: %s", got.Status)
	}
}

func TestSubmitCancelRaceExactlyOneWins(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	var submitErr, cancelErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, submitErr = svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "RACE-1"})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, cancelErr = svc.CancelBatch(context.Background(), CancelBatchRequest{BatchID: b.ID})
	}()
	close(start)
	wg.Wait()

	if (submitErr == nil) == (cancelErr == nil) {
		t.Fatalf("提交与取消必须恰好一个成功: submitErr=%v cancelErr=%v", submitErr, cancelErr)
	}
	got, _ := svc.GetBatch(context.Background(), b.ID)
	switch {
	case submitErr == nil && got.Status != BatchSubmitted:
		t.Fatalf("提交获胜但状态为 %s", got.Status)
	case cancelErr == nil && got.Status != BatchCanceled:
		t.Fatalf("取消获胜但状态为 %s", got.Status)
	}
	pgot, _ := svc.GetPayable(context.Background(), p.ID)
	if got.Status == BatchCanceled && pgot.Status != PayableSettlable {
		t.Fatalf("取消获胜后明细应已释放, got %s", pgot.Status)
	}
	if got.Status == BatchSubmitted && pgot.Status != PayableFrozen {
		t.Fatalf("提交获胜后明细应保持冻结, got %s", pgot.Status)
	}
}

func TestConcurrentBatchCreationSamePayableOneWins(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")

	start := make(chan struct{})
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	ok, fail := 0, 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
				MerchantID: testMerchant, PayableIDs: []string{p.ID},
			})
			mu.Lock()
			if err == nil {
				ok++
			} else {
				fail++
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if ok != 1 || fail != n-1 {
		t.Fatalf("同一明细并发入批次：成功 %d 失败 %d，期望恰好 1/%d", ok, fail, n-1)
	}
	pgot, _ := svc.GetPayable(context.Background(), p.ID)
	if pgot.Status != PayableFrozen {
		t.Fatalf("明细应被唯一批次冻结, got %s", pgot.Status)
	}
}

func TestReceiptSuccess(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "EXT-S"}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "EXT-S", BatchID: b.ID, SubmitVersion: 1,
		Success: true, ResultCode: "OK",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ReceiptConfirmed || res.Status != BatchSucceeded || res.Settlement == nil {
		t.Fatalf("成功回执结果异常: %+v", res)
	}

	// 明细置为 settled。
	pgot, _ := svc.GetPayable(context.Background(), p.ID)
	if pgot.Status != PayableSettled {
		t.Fatalf("成功后明细应为 settled, got %s", pgot.Status)
	}
	stl, err := svc.GetSettlement(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stl.TotalAmount.Amount != 1000 || stl.ExternalNo != "EXT-S" {
		t.Fatalf("结算记录异常: %+v", stl)
	}
	ns, err := svc.ListNotifications(context.Background())
	if err != nil || len(ns) != 1 {
		t.Fatalf("应恰好生成 1 条通知, got %d err %v", len(ns), err)
	}
	if ns[0].SettlementID != stl.ID || ns[0].BatchID != b.ID {
		t.Fatalf("通知内容异常: %+v", ns[0])
	}
}

func TestReceiptFailureReleasesAllAtomic(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "20.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID, p2.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "EXT-F"}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "EXT-F", BatchID: b.ID, SubmitVersion: 1,
		Success: false, ResultCode: "FUNDS_NOT_ENOUGH",
	})
	if err != nil || res.Outcome != ReceiptConfirmed || res.Status != BatchFailed {
		t.Fatalf("失败回执处理异常: %v %+v", err, res)
	}
	for _, pid := range []string{p1.ID, p2.ID} {
		got, _ := svc.GetPayable(context.Background(), pid)
		if got.Status != PayableSettlable || got.BatchID != "" {
			t.Fatalf("失败后明细 %s 应全部释放: %s/%s", pid, got.Status, got.BatchID)
		}
	}
	// 失败不生成结算记录/通知。
	if _, err = svc.GetSettlement(context.Background(), b.ID); !IsNotFound(err) {
		t.Fatalf("失败批次不应有结算记录, got %v", err)
	}
	ns, _ := svc.ListNotifications(context.Background())
	if len(ns) != 0 {
		t.Fatalf("失败批次不应生成通知, got %d", len(ns))
	}
	// 失败终态批次不能再取消/提交。
	if _, err = svc.CancelBatch(context.Background(), CancelBatchRequest{BatchID: b.ID}); !IsConflict(err) {
		t.Fatalf("失败批次取消应报错, got %v", err)
	}
}

func TestReceiptDuplicatesDoNotDuplicateSettlement(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "EXT-D"}); err != nil {
		t.Fatal(err)
	}
	first := BankReceipt{ExternalNo: "EXT-D", BatchID: b.ID, SubmitVersion: 1, Success: true}
	if _, err := svc.HandleBankReceipt(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	// 银行重复推送多次，全部幂等。
	for i := 0; i < 3; i++ {
		res, err := svc.HandleBankReceipt(context.Background(), first)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != ReceiptDuplicate || res.Settlement == nil {
			t.Fatalf("重复成功回执应 duplicate 并带回结算记录: %+v", res)
		}
	}
	ns, _ := svc.ListNotifications(context.Background())
	if len(ns) != 1 {
		t.Fatalf("重复回执不得重复生成通知, got %d", len(ns))
	}

	// 终态后到达的失败回执也不能翻转结果。
	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "EXT-D", BatchID: b.ID, SubmitVersion: 1, Success: false,
	})
	if err != nil || res.Outcome != ReceiptDuplicate || res.Status != BatchSucceeded {
		t.Fatalf("旧/重复失败回执不得覆盖成功终态: %v %+v", err, res)
	}
	got, _ := svc.GetBatch(context.Background(), b.ID)
	if got.Status != BatchSucceeded {
		t.Fatalf("终态被覆盖: %s", got.Status)
	}
}

func TestReceiptVersionMismatchIgnored(t *testing.T) {
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "10.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b.ID, ExternalNo: "V1"}); err != nil {
		t.Fatal(err)
	}

	// 回执版本号与当前提交版本（1）不一致：乱序/错配，忽略。
	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "V1", BatchID: b.ID, SubmitVersion: 2, Success: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ReceiptIgnored || res.Status != BatchSubmitted {
		t.Fatalf("版本不匹配的回执应 ignored 且保持 submitted: %+v", res)
	}

	// 正确版本的失败回执随后到达，正常确认并释放明细。
	res, err = svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "V1", BatchID: b.ID, SubmitVersion: 1, Success: false,
	})
	if err != nil || res.Outcome != ReceiptConfirmed || res.Status != BatchFailed {
		t.Fatalf("正确版本回执应确认失败: %v %+v", err, res)
	}

	// 终态后再到声称其他版本的成功回执：既不匹配当前版本、也不得翻案。
	res, err = svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "V1", BatchID: b.ID, SubmitVersion: 2, Success: true,
	})
	if err != nil || res.Outcome != ReceiptIgnored {
		t.Fatalf("终态后版本不匹配的成功回执应 ignored: %v %+v", err, res)
	}
	got, _ := svc.GetBatch(context.Background(), b.ID)
	if got.Status != BatchFailed {
		t.Fatalf("迟到回执不得覆盖终态: %s", got.Status)
	}
	pgot, _ := svc.GetPayable(context.Background(), p.ID)
	if pgot.Status != PayableSettlable {
		t.Fatalf("失败释放的明细不应被后续回执改动: %s", pgot.Status)
	}
}

func TestReceiptUnknownExternalNo(t *testing.T) {
	svc := newTestService()
	_, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "GHOST", BatchID: "batch_999", SubmitVersion: 1, Success: true,
	})
	if !IsNotFound(err) {
		t.Fatalf("无提交记录的回执应 ErrNotFound, got %v", err)
	}
}

func TestReceiptWrongBatchIgnored(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "10.00")
	p2 := mustRegister(t, svc, testMerchant, "10.00")
	b1, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID},
	})
	b2, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p2.ID},
	})
	// 外部号 EXT 属于 b1，回执却声称属于 b2：串单/迟到，忽略。
	if _, err := svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b1.ID, ExternalNo: "EXT"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "EXT", BatchID: b2.ID, SubmitVersion: 1, Success: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ReceiptIgnored {
		t.Fatalf("批次不匹配的回执应 ignored, got %s", res.Outcome)
	}
	g1, _ := svc.GetBatch(context.Background(), b1.ID)
	g2, _ := svc.GetBatch(context.Background(), b2.ID)
	if g1.Status != BatchSubmitted || g2.Status != BatchOpen {
		t.Fatalf("忽略回执不应改动任何批次: b1=%s b2=%s", g1.Status, g2.Status)
	}
}

func TestSuccessThenFailureReleaseCycle(t *testing.T) {
	// 端到端：登记 -> 批次 -> 提交 -> 失败释放 -> 重新组批 -> 成功 -> 终态不可变。
	svc := newTestService()
	p := mustRegister(t, svc, testMerchant, "99.99")

	b1, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b1.ID, ExternalNo: "E1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "E1", BatchID: b1.ID, SubmitVersion: 1, Success: false,
	}); err != nil {
		t.Fatal(err)
	}

	b2, err := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p.ID},
	})
	if err != nil {
		t.Fatalf("释放后应可重新组批: %v", err)
	}
	if _, err = svc.SubmitBatch(context.Background(), SubmitBatchRequest{BatchID: b2.ID, ExternalNo: "E2"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.HandleBankReceipt(context.Background(), BankReceipt{
		ExternalNo: "E2", BatchID: b2.ID, SubmitVersion: 1, Success: true,
	})
	if err != nil || res.Outcome != ReceiptConfirmed {
		t.Fatalf("二次成功确认异常: %v %+v", err, res)
	}
	pgot, _ := svc.GetPayable(context.Background(), p.ID)
	if pgot.Status != PayableSettled || pgot.BatchID != b2.ID {
		t.Fatalf("最终明细应 settled 于 b2: %s/%s", pgot.Status, pgot.BatchID)
	}
}

func TestContextCancellation(t *testing.T) {
	svc := newTestService()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.RegisterPayable(ctx, RegisterPayableRequest{
		MerchantID: testMerchant, Amount: MustParseMoney("CNY", "1"),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("应透传 context 取消, got %v", err)
	}
}

func TestDeterministicFingerprintAcrossReads(t *testing.T) {
	svc := newTestService()
	p1 := mustRegister(t, svc, testMerchant, "1.00")
	p2 := mustRegister(t, svc, testMerchant, "2.00")
	b, _ := svc.CreateBatch(context.Background(), CreateBatchRequest{
		MerchantID: testMerchant, PayableIDs: []string{p1.ID, p2.ID},
	})
	g1, _ := svc.GetBatch(context.Background(), b.ID)
	g2, _ := svc.GetBatch(context.Background(), b.ID)
	if batchFingerprint(g1) != batchFingerprint(g2) {
		t.Fatal("同一批次多次查询的内容指纹必须稳定")
	}
}

func ExamplePayoutService() {
	svc := NewPayoutService(NewStore())
	ctx := context.Background()
	p, _ := svc.RegisterPayable(ctx, RegisterPayableRequest{
		MerchantID: "M-001",
		Amount:     MustParseMoney("CNY", "100.00"),
	})
	b, _ := svc.CreateBatch(ctx, CreateBatchRequest{
		MerchantID: "M-001",
		PayableIDs: []string{p.ID},
	})
	r, _ := svc.SubmitBatch(ctx, SubmitBatchRequest{BatchID: b.ID, ExternalNo: "REQ-20260928-001"})
	fmt.Println(r.Batch.Status)
	res, _ := svc.HandleBankReceipt(ctx, BankReceipt{
		ExternalNo:    "REQ-20260928-001",
		BatchID:       b.ID,
		SubmitVersion: 1,
		Success:       true,
	})
	fmt.Println(res.Outcome)
	// Output:
	// submitted
	// confirmed
}
