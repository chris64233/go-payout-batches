package gopayoutbatches

import (
	"fmt"
	"sync"
)

// state 是内存数据库的全部数据。Update 在克隆副本上执行事务，
// 提交成功后整体替换，失败则丢弃副本，从而实现"同一持久化边界"语义：
// 业务方法在事务里做的所有修改要么全部生效，要么全部回滚。
type state struct {
	payables      map[string]*Payable
	batches       map[string]*Batch
	submissions   map[string]*Submission // externalNo -> 首次提交记录
	settlements   map[string]*Settlement // batchID -> 唯一结算记录
	notifications []*Notification
	seq           int64
}

func newState() *state {
	return &state{
		payables:      map[string]*Payable{},
		batches:       map[string]*Batch{},
		submissions:   map[string]*Submission{},
		settlements:   map[string]*Settlement{},
		notifications: nil,
	}
}

func (s *state) clone() *state {
	c := newState()
	c.seq = s.seq
	for k, v := range s.payables {
		pv := *v
		c.payables[k] = &pv
	}
	for k, v := range s.batches {
		bv := *v
		bv.Items = append([]BatchItem(nil), v.Items...)
		c.batches[k] = &bv
	}
	for k, v := range s.submissions {
		sv := *v
		c.submissions[k] = &sv
	}
	for k, v := range s.settlements {
		sv := *v
		c.settlements[k] = &sv
	}
	for _, v := range s.notifications {
		nv := *v
		c.notifications = append(c.notifications, &nv)
	}
	return c
}

// Store 线程安全的事务型内存存储。
type Store struct {
	mu sync.RWMutex
	st *state
}

// NewStore 创建空存储。
func NewStore() *Store {
	return &Store{st: newState()}
}

// Tx 事务句柄。事务内拿到的指针仅在 fn 执行期间有效，
// 业务方法必须把需要的数据拷贝出来返回，不得逃逸这些指针。
type Tx struct {
	st *state
}

func (tx *Tx) nextID(prefix string) string {
	tx.st.seq++
	return fmt.Sprintf("%s_%d", prefix, tx.st.seq)
}

// Update 以读写方式执行一个事务。写操作之间互斥；fn 返回错误时整体回滚。
func (s *Store) Update(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.st.clone()
	tx := &Tx{st: work}
	if err := fn(tx); err != nil {
		return err // 直接丢弃 work，s.st 保持原样
	}
	s.st = work
	return nil
}

// View 以只读方式执行一个事务，看到一致性快照。
func (s *Store) View(fn func(tx *Tx) error) error {
	s.mu.RLock()
	st := s.st
	s.mu.RUnlock()
	tx := &Tx{st: st.clone()}
	return fn(tx)
}

// ---- 事务内访问辅助方法 ----

func (tx *Tx) getPayable(id string) (*Payable, bool) {
	p, ok := tx.st.payables[id]
	return p, ok
}

func (tx *Tx) putPayable(p *Payable) { tx.st.payables[p.ID] = p }

func (tx *Tx) getBatch(id string) (*Batch, bool) {
	b, ok := tx.st.batches[id]
	return b, ok
}

func (tx *Tx) putBatch(b *Batch) { tx.st.batches[b.ID] = b }

func (tx *Tx) getSubmission(externalNo string) (*Submission, bool) {
	sub, ok := tx.st.submissions[externalNo]
	return sub, ok
}

func (tx *Tx) putSubmission(sub *Submission) { tx.st.submissions[sub.ExternalNo] = sub }

func (tx *Tx) getSettlementByBatch(batchID string) (*Settlement, bool) {
	stl, ok := tx.st.settlements[batchID]
	return stl, ok
}

func (tx *Tx) putSettlement(stl *Settlement) { tx.st.settlements[stl.BatchID] = stl }

func (tx *Tx) allNotifications() []*Notification { return tx.st.notifications }

func (tx *Tx) appendNotification(n *Notification) {
	tx.st.notifications = append(tx.st.notifications, n)
}
