package gopayoutbatches

import (
	"sync"
	"time"
)

// Store 是进程内的持久化存储（内存实现）。
//
// 所有读改写都必须经由 mutate / mutateT 在同一把互斥锁内完成，
// 该临界区即“同一持久化边界”：批次创建的整批冻结、提交与取消的
// 竞争、失败回执释放全部明细等原子性都由它保证。未来替换成数据库时，
// 只需把 mutate 的回调整体放进一个数据库事务即可。
type Store struct {
	mu sync.Mutex

	seq int64

	items   map[string]*PayableItem
	batches map[string]*Batch

	// itemBizIndex: (merchantID, bizNo) -> itemID，登记幂等。
	itemBizIndex map[[2]string]string
	// externalIndex: 外部提交号 -> 批次ID。一个外部号一旦使用即与
	// 唯一批次绑定，重发只接受同批次同内容。
	externalIndex map[string]string
	// submissionIndex: (externalNo, version) -> 批次ID，
	// 用于回执定位“当前批次 + 提交版本”。
	submissionIndex map[[2]interface{}]string

	settlements    map[string]*Settlement
	notifications  map[string]*Notification
	notificationID map[string]string // batchID -> notificationID

	now func() time.Time
}

// NewStore 创建空存储。
func NewStore() *Store {
	return &Store{
		items:           map[string]*PayableItem{},
		batches:         map[string]*Batch{},
		itemBizIndex:    map[[2]string]string{},
		externalIndex:   map[string]string{},
		submissionIndex: map[[2]interface{}]string{},
		settlements:     map[string]*Settlement{},
		notifications:   map[string]*Notification{},
		notificationID:  map[string]string{},
		now:             time.Now,
	}
}

// mutate 在一个原子持久化边界内执行 fn（带返回值）。
func mutateT[T any](s *Store, fn func() (T, error)) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

// mutate 在一个原子持久化边界内执行 fn。
func (s *Store) mutate(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

func (s *Store) nextID(prefix string) string {
	s.seq++
	return prefix + itoa(s.seq)
}

// itoa 避免引入 strconv 的轻量整数格式化。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 深拷贝：存储返回给上层的对象一律是副本，外部修改不会污染内部状态。
// ---------------------------------------------------------------------------

func cloneItem(it *PayableItem) *PayableItem {
	if it == nil {
		return nil
	}
	c := *it
	return &c
}

func cloneBatch(b *Batch) *Batch {
	if b == nil {
		return nil
	}
	c := *b
	c.Items = append([]BatchItem(nil), b.Items...)
	if b.Submission != nil {
		sub := *b.Submission
		c.Submission = &sub
	}
	if b.Settlement != nil {
		st := *b.Settlement
		c.Settlement = &st
	}
	return &c
}

func cloneSettlement(st *Settlement) *Settlement {
	if st == nil {
		return nil
	}
	c := *st
	return &c
}

func cloneNotification(n *Notification) *Notification {
	if n == nil {
		return nil
	}
	c := *n
	return &c
}
