package state

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ledgerwatch/erigon-lib/commitment"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/length"
	"github.com/ledgerwatch/erigon-lib/etl"
)

func (a *SharedDomains) ComputeCommitment(txNum uint64, pk, hk [][]byte, upd []commitment.Update, saveStateAfter, trace bool) (rootHash []byte, err error) {
	// if commitment mode is Disabled, there will be nothing to compute on.
	//mxCommitmentRunning.Inc()
	rootHash, branchNodeUpdates, err := a.Commitment.ComputeCommitment(pk, hk, upd, trace)
	//mxCommitmentRunning.Dec()
	if err != nil {
		return nil, err
	}
	//if a.seekTxNum > a.txNum {
	//	saveStateAfter = false
	//}

	//mxCommitmentKeys.Add(int(a.commitment.comKeys))
	//mxCommitmentTook.Update(a.commitment.comTook.Seconds())

	defer func(t time.Time) { mxCommitmentWriteTook.UpdateDuration(t) }(time.Now())

	//sortedPrefixes := make([]string, len(branchNodeUpdates))
	//for pref := range branchNodeUpdates {
	//	sortedPrefixes = append(sortedPrefixes, pref)
	//}
	//sort.Strings(sortedPrefixes)

	cct := a.Commitment //.MakeContext()
	//defer cct.Close()

	for pref, update := range branchNodeUpdates {
		prefix := []byte(pref)
		//update := branchNodeUpdates[pref]

		stateValue, err := cct.Get(prefix, nil)
		if err != nil {
			return nil, err
		}
		//mxCommitmentUpdates.Inc()
		stated := commitment.BranchData(stateValue)
		merged, err := a.Commitment.c.branchMerger.Merge(stated, update)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(stated, merged) {
			continue
		}
		if trace {
			fmt.Printf("computeCommitment merge [%x] [%x]+[%x]=>[%x]\n", prefix, stated, update, merged)
		}
		if err = a.Commitment.Put(prefix, nil, merged); err != nil {
			return nil, err
		}
		//mxCommitmentUpdatesApplied.Inc()
	}

	if saveStateAfter {
		if err := a.Commitment.c.storeCommitmentState(0, txNum); err != nil {
			return nil, err
		}
	}

	return rootHash, nil
}

type SharedDomains struct {
	Account    *DomainMem
	Storage    *DomainMem
	Code       *DomainMem
	Commitment *DomainMemCommit

	Updates *UpdateTree
}

type DomainMemCommit struct {
	*DomainMem
	c *DomainCommitted
}

func (d *DomainMemCommit) ComputeCommitment(pk, hk [][]byte, upd []commitment.Update, trace bool) (rootHash []byte, branchNodeUpdates map[string]commitment.BranchData, err error) {
	return d.c.CommitmentOver(pk, hk, upd, trace)
}

func NewSharedDomains(tmp string, a, c, s *Domain, comm *DomainCommitted) *SharedDomains {
	return &SharedDomains{
		Updates:    NewUpdateTree(comm.mode),
		Account:    NewDomainMem(a, tmp),
		Storage:    NewDomainMem(s, tmp),
		Code:       NewDomainMem(c, tmp),
		Commitment: &DomainMemCommit{DomainMem: NewDomainMem(comm.Domain, tmp), c: comm},
	}
}

func (s *SharedDomains) BranchFn(pref []byte) ([]byte, error) {
	v, err := s.Commitment.Get(pref, nil)
	if err != nil {
		return nil, fmt.Errorf("branchFn: no value for prefix %x: %w", pref, err)
	}
	// skip touchmap
	return v[2:], nil
}

func (s *SharedDomains) AccountFn(plainKey []byte, cell *commitment.Cell) error {
	encAccount, err := s.Account.Get(plainKey, nil)
	if err != nil {
		return fmt.Errorf("accountFn: no value for address %x : %w", plainKey, err)
	}
	cell.Nonce = 0
	cell.Balance.Clear()
	copy(cell.CodeHash[:], commitment.EmptyCodeHash)
	if len(encAccount) > 0 {
		nonce, balance, chash := DecodeAccountBytes(encAccount)
		cell.Nonce = nonce
		cell.Balance.Set(balance)
		if chash != nil {
			copy(cell.CodeHash[:], chash)
		}
	}

	code, _ := s.Code.Get(plainKey, nil)
	if code != nil {
		s.Updates.keccak.Reset()
		s.Updates.keccak.Write(code)
		copy(cell.CodeHash[:], s.Updates.keccak.Sum(nil))
	}
	cell.Delete = len(encAccount) == 0 && len(code) == 0
	return nil
}

func (s *SharedDomains) StorageFn(plainKey []byte, cell *commitment.Cell) error {
	// Look in the summary table first
	enc, err := s.Storage.Get(plainKey[:length.Addr], plainKey[length.Addr:])
	if err != nil {
		return err
	}
	cell.StorageLen = len(enc)
	copy(cell.Storage[:], enc)
	cell.Delete = cell.StorageLen == 0
	return nil
}

type DomainMem struct {
	*Domain

	etl    *etl.Collector
	mu     sync.RWMutex
	values map[string]*KVList
	latest map[string][]byte // key+^step -> value
}

type KVList struct {
	TxNum []uint64
	//Keys  []string
	Vals [][]byte
}

func (l *KVList) Latest() (tx uint64, v []byte) {
	sz := len(l.TxNum)
	if sz == 0 {
		return 0, nil
	}
	sz--

	tx = l.TxNum[sz]
	v = l.Vals[sz]
	return tx, v
}

func (l *KVList) Put(tx uint64, v []byte) (prevTx uint64, prevV []byte) {
	prevTx, prevV = l.Latest()
	l.TxNum = append(l.TxNum, tx)
	l.Vals = append(l.Vals, v)
	return
}

func (l *KVList) Len() int {
	return len(l.TxNum)
}

func (l *KVList) Apply(f func(txn uint64, v []byte) error) error {
	for i, tx := range l.TxNum {
		if err := f(tx, l.Vals[i]); err != nil {
			return err
		}
	}
	return nil
}

func (l *KVList) Reset() {
	//l.Keys = l.Keys[:0]
	l.TxNum = l.TxNum[:0]
	l.Vals = l.Vals[:0]
}

func NewDomainMem(d *Domain, tmpdir string) *DomainMem {
	return &DomainMem{
		Domain: d,
		latest: make(map[string][]byte, 128),
		etl:    etl.NewCollector(d.valsTable, tmpdir, etl.NewSortableBuffer(WALCollectorRam)),
		//values: &KVList{
		//	Keys: make([]string, 0, 1000),
		//	Vals: make([][]byte, 0, 1000),
		//},
		values: make(map[string]*KVList, 128),
	}
}

func (d *DomainMem) Get(k1, k2 []byte) ([]byte, error) {
	key := common.Append(k1, k2)

	d.mu.RLock()
	//value, _ := d.latest[string(key)]
	value, ok := d.values[string(key)]
	d.mu.RUnlock()

	if ok {
		_, v := value.Latest()
		return v, nil
	}
	return nil, nil
}

// TODO:
// 1. Add prev value to WAL
// 2. read prev value correctly from domain
// 3. load from etl to table, process on the fly to avoid domain pruning

func (d *DomainMem) Flush() {
	err := d.etl.Load(d.tx, d.valsTable, d.etlLoader(), etl.TransformArgs{})
	if err != nil {
		panic(err)
	}
}

func (d *DomainMem) Close() {
	d.etl.Close()
}

func (d *DomainMem) etlLoader() etl.LoadFunc {
	stepSize := d.aggregationStep
	//assert := func(k []byte) {
	//	if
	//}
	return func(k []byte, value []byte, _ etl.CurrentTableReader, next etl.LoadNextFunc) error {
		// if its ordered we could put to history each key excluding last one
		tx := binary.BigEndian.Uint64(k[len(k)-8:])

		keySuffix := make([]byte, len(k))
		binary.BigEndian.PutUint64(keySuffix[len(k)-8:], ^(tx / stepSize))
		var k2 []byte
		if len(k) > length.Addr+8 {
			k2 = k[length.Addr : len(k)-8]
		}

		if err := d.Put(k[:length.Addr], k2, value); err != nil {
			return err
		}
		return next(k, keySuffix, value)
	}
}

func (d *DomainMem) Put(k1, k2, value []byte) error {
	key := common.Append(k1, k2)
	ks := *(*string)(unsafe.Pointer(&key))

	invertedStep := ^(d.txNum / d.aggregationStep)
	keySuffix := make([]byte, len(key)+8)
	copy(keySuffix, key)
	binary.BigEndian.PutUint64(keySuffix[len(key):], invertedStep)

	if err := d.etl.Collect(keySuffix, value); err != nil {
		return err
	}

	d.mu.Lock()
	kvl, ok := d.values[ks]
	if !ok {
		kvl = &KVList{
			TxNum: make([]uint64, 0, 10),
			Vals:  make([][]byte, 0, 10),
		}
		d.values[ks] = kvl
	}

	ltx, prev := d.values[ks].Put(d.txNum, value)
	_ = ltx
	d.mu.Unlock()

	if len(prev) == 0 {
		var ok bool
		prev, ok = d.defaultDc.readFromFiles(key, 0)
		if !ok {
			return fmt.Errorf("failed to read from files: %x", key)
		}
	}

	if err := d.wal.addPrevValue(k1, k2, prev); err != nil {
		return err
	}

	return nil
	//return d.PutWitPrev(k1, k2, value, prev)
}

func (d *DomainMem) Delete(k1, k2 []byte) error {
	if err := d.Put(k1, k2, nil); err != nil {
		return err
	}
	return nil
	//key := common.Append(k1, k2)
	//return d.DeleteWithPrev(k1, k2, prev)
}

func (d *DomainMem) Reset() {
	d.mu.Lock()
	d.latest = make(map[string][]byte)
	//d.values.Reset()
	d.mu.Unlock()
}

//type UpdateWriter *UpdateTree
//
//func (w *(*UpdateWriter)) UpdateAccountData(address common.Address, original, account *accounts.Account) error {
//	//TODO implement me
//	w.TouchPlainKey(addressBytes, value, w.rs.Commitment.TouchPlainKeyAccount)
//	panic("implement me")
//}
//
//func (UpdateWriter) UpdateAccountCode(address common.Address, incarnation uint64, codeHash common.Hash, code []byte) error {
//	//TODO implement me
//	panic("implement me")
//}
//
//func (UpdateWriter) DeleteAccount(address common.Address, original *accounts.Account) error {
//	//TODO implement me
//	panic("implement me")
//}
//
//func (UpdateWriter) WriteAccountStorage(address common.Address, incarnation uint64, key *common.Hash, original, value *uint256.Int) error {
//	//TODO implement me
//	panic("implement me")
//}
//
//func (UpdateWriter) CreateContract(address common.Address) error {
//	//TODO implement me
//	panic("implement me")
//}
