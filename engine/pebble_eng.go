package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/youzan/ZanRedisDB/common"
)

var (
	errDBEngClosed = errors.New("db engine is closed")
	errIntNumber   = errors.New("invalid integer")
)

const (
	numOfLevels = 7
)

type pebbleRefSlice struct {
	b []byte
	c io.Closer
}

func (rs *pebbleRefSlice) Free() {
}

func (rs *pebbleRefSlice) Data() []byte {
	return rs.b
}

func GetRocksdbUint64(v []byte, err error) (uint64, error) {
	if err != nil {
		return 0, err
	} else if v == nil || len(v) == 0 {
		return 0, nil
	} else if len(v) != 8 {
		return 0, errIntNumber
	}

	return binary.LittleEndian.Uint64(v), nil
}

type Uint64AddMerger struct {
	buf []byte
}

func (m *Uint64AddMerger) MergeNewer(value []byte) error {
	cur, err := GetRocksdbUint64(m.buf, nil)
	if err != nil {
		return err
	}
	vint, err := GetRocksdbUint64(value, nil)
	if err != nil {
		return err
	}
	nv := cur + vint
	if m.buf == nil {
		m.buf = make([]byte, 8)
	}
	binary.LittleEndian.PutUint64(m.buf, nv)
	return nil
}

func (m *Uint64AddMerger) MergeOlder(value []byte) error {
	return m.MergeNewer(value)
}

func (m *Uint64AddMerger) Finish() ([]byte, io.Closer, error) {
	return m.buf, nil, nil
}

func newUint64AddMerger() *pebble.Merger {
	return &pebble.Merger{
		Merge: func(key, value []byte) (pebble.ValueMerger, error) {
			res := &Uint64AddMerger{}
			res.MergeNewer(value)
			return res, nil
		},
		Name: "pebble.uint64add",
	}
}

type PebbleEng struct {
	rwmutex     sync.RWMutex
	cfg         *RockEngConfig
	eng         *pebble.DB
	opts        *pebble.Options
	wo          *pebble.WriteOptions
	ito         *pebble.IterOptions
	wb          *pebbleWriteBatch
	engOpened   int32
	lastCompact int64
	deletedCnt  int64
	quit        chan struct{}
}

func NewPebbleEng(cfg *RockEngConfig) (*PebbleEng, error) {
	if len(cfg.DataDir) == 0 {
		return nil, errors.New("config error")
	}

	err := os.MkdirAll(cfg.DataDir, common.DIR_PERM)
	if err != nil {
		return nil, err
	}
	lopts := make([]pebble.LevelOptions, 0)
	for l := 0; l < numOfLevels; l++ {
		compress := pebble.SnappyCompression
		if l <= cfg.MinLevelToCompress {
			compress = pebble.NoCompression
		}
		filter := bloom.FilterPolicy(10)
		opt := pebble.LevelOptions{
			Compression:    compress,
			BlockSize:      cfg.BlockSize,
			TargetFileSize: int64(cfg.TargetFileSizeBase),
			FilterPolicy:   filter,
		}
		opt.EnsureDefaults()
		lopts = append(lopts, opt)
	}

	opts := &pebble.Options{
		Levels:                      lopts,
		MaxManifestFileSize:         int64(cfg.MaxMainifestFileSize),
		MemTableSize:                cfg.WriteBufferSize,
		MemTableStopWritesThreshold: cfg.MaxWriteBufferNumber,
		LBaseMaxBytes:               int64(cfg.MaxBytesForLevelBase),
		L0CompactionThreshold:       cfg.Level0FileNumCompactionTrigger,
		MaxOpenFiles:                -1,
		MaxConcurrentCompactions:    cfg.MaxBackgroundCompactions,
	}
	// prefix search
	opts.Comparer = pebble.DefaultComparer
	opts.Comparer.Split = func(a []byte) int {
		if len(a) <= 3 {
			return len(a)
		}
		return 3
	}
	if !cfg.DisableMergeCounter {
		if cfg.EnableTableCounter {
			opts.Merger = newUint64AddMerger()
		}
	} else {
		cfg.EnableTableCounter = false
	}
	db := &PebbleEng{
		cfg:  cfg,
		opts: opts,
		ito:  &pebble.IterOptions{},
		wo: &pebble.WriteOptions{
			Sync: false,
		},
		quit: make(chan struct{}),
	}

	return db, nil
}

func (pe *PebbleEng) NewWriteBatch() WriteBatch {
	return newPebbleWriteBatch(pe.eng, pe.wo)
}

func (pe *PebbleEng) DefaultWriteBatch() WriteBatch {
	return pe.wb
}

func (pe *PebbleEng) GetDataDir() string {
	return path.Join(pe.cfg.DataDir, "pebble")
}

func (pe *PebbleEng) SetMaxBackgroundOptions(maxCompact int, maxBackJobs int) error {
	return nil
}

func (pe *PebbleEng) CheckDBEngForRead(fullPath string) error {
	ro := *(pe.opts)
	ro.ErrorIfNotExists = true
	ro.ReadOnly = true
	db, err := pebble.Open(fullPath, &ro)
	if err != nil {
		return err
	}
	db.Close()
	return nil
}

func (pe *PebbleEng) OpenEng() error {
	if !pe.IsClosed() {
		dbLog.Warningf("engine already opened: %v, should close it before reopen", pe.GetDataDir())
		return errors.New("open failed since not closed")
	}
	cache := pebble.NewCache(pe.cfg.BlockCache)
	pe.opts.Cache = cache
	eng, err := pebble.Open(pe.GetDataDir(), pe.opts)
	if err != nil {
		return err
	}
	cache.Unref()
	pe.eng = eng
	pe.wb = newPebbleWriteBatch(eng, pe.wo)
	atomic.StoreInt32(&pe.engOpened, 1)
	dbLog.Infof("engine opened: %v", pe.GetDataDir())
	return nil
}

func (pe *PebbleEng) Write(wb WriteBatch) error {
	pwb := wb.(*pebbleWriteBatch)
	return pe.eng.Apply(pwb.wb, pe.wo)
}

func (pe *PebbleEng) DeletedBeforeCompact() int64 {
	return atomic.LoadInt64(&pe.deletedCnt)
}

func (pe *PebbleEng) AddDeletedCnt(c int64) {
	atomic.AddInt64(&pe.deletedCnt, c)
}

func (pe *PebbleEng) LastCompactTime() int64 {
	return atomic.LoadInt64(&pe.lastCompact)
}

func (pe *PebbleEng) CompactRange(rg CRange) {
	atomic.StoreInt64(&pe.lastCompact, time.Now().Unix())
	atomic.StoreInt64(&pe.deletedCnt, 0)
	pe.rwmutex.RLock()
	closed := pe.IsClosed()
	pe.rwmutex.RUnlock()
	if closed {
		return
	}
	pe.eng.Compact(rg.Start, rg.Limit)
}

func (pe *PebbleEng) CompactAllRange() {
	pe.CompactRange(CRange{})
}

func (pe *PebbleEng) GetApproximateTotalKeyNum() int {
	return 0
}

func (pe *PebbleEng) GetApproximateKeyNum(ranges []CRange) uint64 {
	return 0
}

func (pe *PebbleEng) GetApproximateSizes(ranges []CRange, includeMem bool) []uint64 {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	sizeList := make([]uint64, len(ranges))
	if pe.IsClosed() {
		return sizeList
	}
	for i, r := range ranges {
		sizeList[i], _ = pe.eng.EstimateDiskUsage(r.Start, r.Limit)
	}
	return sizeList
}

func (pe *PebbleEng) IsClosed() bool {
	if atomic.LoadInt32(&pe.engOpened) == 0 {
		return true
	}
	return false
}

func (pe *PebbleEng) CloseEng() bool {
	pe.rwmutex.Lock()
	defer pe.rwmutex.Unlock()
	if pe.eng != nil {
		if atomic.CompareAndSwapInt32(&pe.engOpened, 1, 0) {
			if pe.wb != nil {
				pe.wb.Destroy()
			}
			pe.eng.Close()
			dbLog.Infof("engine closed: %v", pe.GetDataDir())
			return true
		}
	}
	return false
}

func (pe *PebbleEng) CloseAll() {
	select {
	case <-pe.quit:
	default:
		close(pe.quit)
	}
	pe.CloseEng()
}

func (pe *PebbleEng) GetStatistics() string {
	return pe.eng.Metrics().String()
}

func (pe *PebbleEng) GetInternalStatus() map[string]interface{} {
	return nil
}

func (pe *PebbleEng) GetInternalPropertyStatus(p string) string {
	return p
}

func (pe *PebbleEng) GetBytesNoLock(key []byte) ([]byte, error) {
	val, c, err := pe.eng.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		return nil, err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	if val == nil {
		return nil, nil
	}
	b := make([]byte, len(val))
	copy(b, val)
	return b, nil
}

func (pe *PebbleEng) GetBytes(key []byte) ([]byte, error) {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	if pe.IsClosed() {
		return nil, errDBEngClosed
	}
	return pe.GetBytesNoLock(key)
}

func (pe *PebbleEng) MultiGetBytes(keyList [][]byte, values [][]byte, errs []error) {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	if pe.IsClosed() {
		for i, _ := range errs {
			errs[i] = errDBEngClosed
		}
		return
	}
	for i, k := range keyList {
		values[i], errs[i] = pe.GetBytesNoLock(k)
	}
}

func (pe *PebbleEng) Exist(key []byte) (bool, error) {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	if pe.IsClosed() {
		return false, errDBEngClosed
	}
	return pe.ExistNoLock(key)
}

func (pe *PebbleEng) ExistNoLock(key []byte) (bool, error) {
	val, c, err := pe.eng.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		return false, err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	return val != nil, nil
}

func (pe *PebbleEng) GetRef(key []byte) (*pebbleRefSlice, error) {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	if pe.IsClosed() {
		return nil, errDBEngClosed
	}
	val, c, err := pe.eng.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		return nil, err
	}
	return &pebbleRefSlice{b: val, c: c}, nil
}

func (pe *PebbleEng) GetValueWithOp(key []byte,
	op func([]byte) error) error {
	pe.rwmutex.RLock()
	defer pe.rwmutex.RUnlock()
	if pe.IsClosed() {
		return errDBEngClosed
	}
	val, c, err := pe.eng.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		return err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	return op(val)
}

func (pe *PebbleEng) GetValueWithOpNoLock(key []byte,
	op func([]byte) error) error {
	val, c, err := pe.eng.Get(key)
	if err != nil && err != pebble.ErrNotFound {
		return err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	return op(val)
}

func (pe *PebbleEng) GetIterator(opts IteratorOpts) (Iterator, error) {
	dbit, err := newPebbleIterator(pe, opts)
	if err != nil {
		return nil, err
	}
	return dbit, nil
}

func (pe *PebbleEng) NewCheckpoint() (KVCheckpoint, error) {
	return &pebbleEngCheckpoint{
		eng: pe,
	}, nil
}

type pebbleEngCheckpoint struct {
	eng *PebbleEng
}

func (pck *pebbleEngCheckpoint) Save(path string, notify chan struct{}) error {
	pck.eng.rwmutex.RLock()
	defer pck.eng.rwmutex.RUnlock()
	if pck.eng.IsClosed() {
		return errDBEngClosed
	}
	time.AfterFunc(time.Millisecond*20, func() {
		close(notify)
	})
	return pck.eng.eng.Checkpoint(path)
}
