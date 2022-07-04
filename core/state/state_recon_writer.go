package state

import (
	//"fmt"

	"bytes"
	"container/heap"
	"encoding/binary"
	"sync"
	"unsafe"

	"github.com/RoaringBitmap/roaring/roaring64"
	"github.com/google/btree"
	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/kv"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/dbutils"
	"github.com/ledgerwatch/erigon/core/types/accounts"
	"golang.org/x/exp/constraints"
)

type theap[T constraints.Ordered] []T

func (h theap[T]) Len() int {
	return len(h)
}

func (h theap[T]) Less(i, j int) bool {
	return h[i] < h[j]
}

func (h theap[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *theap[T]) Push(a interface{}) {
	*h = append(*h, a.(T))
}

func (h *theap[T]) Pop() interface{} {
	c := *h
	*h = c[:len(c)-1]
	return c[len(c)-1]
}

type ReconStateItem struct {
	txNum      uint64 // txNum where the item has been created
	key1, key2 []byte
	val        []byte
}

func (i ReconStateItem) Less(than btree.Item) bool {
	thanItem := than.(ReconStateItem)
	if i.txNum == thanItem.txNum {
		c1 := bytes.Compare(i.key1, thanItem.key1)
		if c1 == 0 {
			c2 := bytes.Compare(i.key2, thanItem.key2)
			return c2 < 0
		}
		return c1 < 0
	}
	return i.txNum < thanItem.txNum
}

// ReconState is the accumulator of changes to the state
type ReconState struct {
	lock          sync.RWMutex
	workIterator  roaring64.IntPeekable64
	doneBitmap    roaring64.Bitmap
	triggers      map[uint64][]uint64
	queue         theap[uint64]
	changes       map[string]*btree.BTree // table => [] (txNum; key1; key2; val)
	sizeEstimate  uint64
	rollbackCount uint64
}

func NewReconState() *ReconState {
	rs := &ReconState{
		triggers: map[uint64][]uint64{},
		changes:  map[string]*btree.BTree{},
	}
	return rs
}

func (rs *ReconState) SetWorkBitmap(workBitmap *roaring64.Bitmap) {
	rs.workIterator = workBitmap.Iterator()
}

func (rs *ReconState) Put(table string, key1, key2, val []byte, txNum uint64) {
	rs.lock.Lock()
	defer rs.lock.Unlock()
	t, ok := rs.changes[table]
	if !ok {
		t = btree.New(32)
		rs.changes[table] = t
	}
	item := ReconStateItem{key1: key1, key2: key2, val: val, txNum: txNum}
	t.ReplaceOrInsert(item)
	rs.sizeEstimate += uint64(unsafe.Sizeof(item)) + uint64(len(key1)) + uint64(len(key2)) + uint64(len(val))
}

func (rs *ReconState) Get(table string, key1, key2 []byte, txNum uint64) []byte {
	rs.lock.RLock()
	defer rs.lock.RUnlock()
	t, ok := rs.changes[table]
	if !ok {
		return nil
	}
	i := t.Get(ReconStateItem{txNum: txNum, key1: key1, key2: key2})
	if i == nil {
		return nil
	}
	return i.(ReconStateItem).val
}

func (rs *ReconState) Flush(rwTx kv.RwTx) error {
	rs.lock.Lock()
	defer rs.lock.Unlock()
	for table, t := range rs.changes {
		var err error
		t.Ascend(func(i btree.Item) bool {
			item := i.(ReconStateItem)
			if len(item.val) == 0 {
				return true
			}
			var composite []byte
			if item.key2 == nil {
				composite = make([]byte, 8+len(item.key1))
			} else {
				composite = make([]byte, 8+len(item.key1)+8+len(item.key2))
				binary.BigEndian.PutUint64(composite[8+len(item.key1):], 1)
				copy(composite[8+len(item.key1)+8:], item.key2)
			}
			binary.BigEndian.PutUint64(composite, item.txNum)
			copy(composite[8:], item.key1)
			if err = rwTx.Put(table, composite, item.val); err != nil {
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		t.Clear(true)
	}
	rs.sizeEstimate = 0
	return nil
}

func (rs *ReconState) Schedule() (uint64, bool) {
	rs.lock.Lock()
	defer rs.lock.Unlock()
	for rs.queue.Len() < 16 && rs.workIterator.HasNext() {
		heap.Push(&rs.queue, rs.workIterator.Next())
	}
	if rs.queue.Len() > 0 {
		return heap.Pop(&rs.queue).(uint64), true
	}
	return 0, false
}

func (rs *ReconState) CommitTxNum(txNum uint64) {
	rs.lock.Lock()
	defer rs.lock.Unlock()
	if tt, ok := rs.triggers[txNum]; ok {
		for _, t := range tt {
			heap.Push(&rs.queue, t)
		}
		delete(rs.triggers, txNum)
	}
	rs.doneBitmap.Add(txNum)
}

func (rs *ReconState) RollbackTxNum(txNum, dependency uint64) {
	rs.lock.Lock()
	defer rs.lock.Unlock()
	if rs.doneBitmap.Contains(dependency) {
		heap.Push(&rs.queue, txNum)
	} else {
		tt, _ := rs.triggers[dependency]
		tt = append(tt, txNum)
		rs.triggers[dependency] = tt
	}
	rs.rollbackCount++
}

func (rs *ReconState) Done(txNum uint64) bool {
	rs.lock.RLock()
	defer rs.lock.RUnlock()
	return rs.doneBitmap.Contains(txNum)
}

func (rs *ReconState) DoneCount() uint64 {
	rs.lock.RLock()
	defer rs.lock.RUnlock()
	return rs.doneBitmap.GetCardinality()
}

func (rs *ReconState) RollbackCount() uint64 {
	rs.lock.RLock()
	defer rs.lock.RUnlock()
	return rs.rollbackCount
}

func (rs *ReconState) SizeEstimate() uint64 {
	rs.lock.RLock()
	defer rs.lock.RUnlock()
	return rs.sizeEstimate
}

type StateReconWriter struct {
	ac    *libstate.AggregatorContext
	rs    *ReconState
	txNum uint64
}

func NewStateReconWriter(ac *libstate.AggregatorContext, rs *ReconState) *StateReconWriter {
	return &StateReconWriter{
		ac: ac,
		rs: rs,
	}
}

func (w *StateReconWriter) SetTxNum(txNum uint64) {
	w.txNum = txNum
}

func (w *StateReconWriter) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
	found, txNum := w.ac.MaxAccountsTxNum(address.Bytes())
	if !found {
		return nil
	}
	if txNum != w.txNum {
		//fmt.Printf("no change account [%x] txNum = %d\n", address, txNum)
		return nil
	}
	value := make([]byte, account.EncodingLengthForStorage())
	account.EncodeForStorage(value)
	//fmt.Printf("account [%x]=>{Balance: %d, Nonce: %d, Root: %x, CodeHash: %x} txNum: %d\n", address, &account.Balance, account.Nonce, account.Root, account.CodeHash, w.txNum)
	w.rs.Put(kv.PlainStateR, address[:], nil, value, w.txNum)
	return nil
}

func (w *StateReconWriter) UpdateAccountCode(address common.Address, incarnation uint64, codeHash common.Hash, code []byte) error {
	found, txNum := w.ac.MaxCodeTxNum(address.Bytes())
	if !found {
		return nil
	}
	if txNum != w.txNum {
		//fmt.Printf("no change code [%x] txNum = %d\n", address, txNum)
		return nil
	}
	w.rs.Put(kv.CodeR, codeHash[:], nil, code, w.txNum)
	if len(code) > 0 {
		//fmt.Printf("code [%x] => [%x] CodeHash: %x, txNum: %d\n", address, code, codeHash, w.txNum)
		w.rs.Put(kv.PlainContractR, dbutils.PlainGenerateStoragePrefix(address[:], FirstContractIncarnation), nil, codeHash[:], w.txNum)
	}
	return nil
}

func (w *StateReconWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
	return nil
}

func (w *StateReconWriter) WriteAccountStorage(address common.Address, incarnation uint64, key *common.Hash, original, value *uint256.Int) error {
	found, txNum := w.ac.MaxStorageTxNum(address.Bytes(), key.Bytes())
	if !found {
		//fmt.Printf("no found storage [%x] [%x]\n", address, *key)
		return nil
	}
	if txNum != w.txNum {
		//fmt.Printf("no change storage [%x] [%x] txNum = %d\n", address, *key, txNum)
		return nil
	}
	v := value.Bytes()
	if len(v) != 0 {
		//fmt.Printf("storage [%x] [%x] => [%x], txNum: %d\n", address, *key, v, w.txNum)
		w.rs.Put(kv.PlainStateR, address.Bytes(), key.Bytes(), v, w.txNum)
	}
	return nil
}

func (w *StateReconWriter) CreateContract(address common.Address) error {
	return nil
}
