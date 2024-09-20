package lotusdb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/dgraph-io/badger/v4/y"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rosedblabs/diskhash"
	"github.com/rosedblabs/wal"
	"golang.org/x/sync/errgroup"
)

const (
	fileLockName       = "FLOCK"
	deprecatedMetaName = "DEPMETA"
)

// DB is the main structure of the LotusDB database.
// It contains all the information needed to operate the database.
//
// DB is thread-safe, and can be used by multiple goroutines.
// But you can not open multiple DBs with the same directory path at the same time.
// ErrDatabaseIsUsing will be returned if you do so.
//
// LotusDB is the most advanced key-value database written in Go.
// It combines the advantages of LSM tree and B+ tree, read and write are both very fast.
// It is also very memory efficient, and can store billions of key-value pairs in a single machine.
type DB struct {
	activeMem        *memtable            // Active memtable for writing.
	immuMems         []*memtable          // Immutable memtables, waiting to be flushed to disk.
	index            Index                // index is multi-partition indexes to store key and chunk position.
	vlog             *valueLog            // vlog is the value log.
	fileLock         *flock.Flock         // fileLock to prevent multiple processes from using the same database directory.
	flushChan        chan *memtable       // flushChan is used to notify the flush goroutine to flush memtable to disk.
	flushLock        sync.Mutex           // flushLock is to prevent flush running while compaction doesn't occur
	compactChan      chan deprecatedState // compactChan is used to notify the shard need to compact
	diskIO           *DiskIO              // monitoring the IO status of disks and allowing autoCompact when appropriate
	mu               sync.RWMutex
	closed           bool
	closeflushChan   chan struct{}
	closeCompactChan chan struct{}
	options          Options
	batchPool        sync.Pool // batchPool is a pool of batch, to reduce the cost of memory allocation.
}

// Open a database with the specified options.
// If the database directory does not exist, it will be created automatically.
//
// Multiple processes can not use the same database directory at the same time,
// otherwise it will return ErrDatabaseIsUsing.
//
// It will first open the wal to rebuild the memtable, then open the index and value log.
// Return the DB object if succeeded, otherwise return the error.
//
//nolint:funlen // default
func Open(options Options) (*DB, error) {
	// check whether all options are valid
	if err := validateOptions(&options); err != nil {
		return nil, err
	}

	// create data directory if not exist
	if _, err := os.Stat(options.DirPath); err != nil {
		if err = os.MkdirAll(options.DirPath, os.ModePerm); err != nil {
			return nil, err
		}
	}

	// create file lock, prevent multiple processes from using the same database directory
	fileLock := flock.New(filepath.Join(options.DirPath, fileLockName))
	hold, err := fileLock.TryLock()
	if err != nil {
		return nil, err
	}
	if !hold {
		return nil, ErrDatabaseIsUsing
	}

	// create deprecatedMeta file if not exist, read deprecatedNumber
	deprecatedMetaPath := filepath.Join(options.DirPath, deprecatedMetaName)
	var deprecatedNumber uint32
	var totalEntryNumber uint32
	//nolint:nestif // The situation here requires more judgments, so there are quite a few nested conditions.
	if _, err = os.Stat(deprecatedMetaPath); os.IsNotExist(err) {
		var file *os.File
		file, err = os.Create(deprecatedMetaPath)
		if err != nil {
			return nil, err
		}
		deprecatedNumber = 0
		totalEntryNumber = 0
		defer file.Close()
	} else if err != nil {
		return nil, err
	} else {
		var file *os.File
		file, err = os.Open(deprecatedMetaPath)
		if err != nil {
			return nil, err
		}
		// 读取deprecatedNumber
		err = binary.Read(file, binary.LittleEndian, &deprecatedNumber)
		if err != nil {
			// 处理错误
			return nil, err
		}

		// 读取totalEntryNumber
		err = binary.Read(file, binary.LittleEndian, &totalEntryNumber)
		if err != nil {
			// 处理错误
			return nil, err
		}

		// 打印读取的值
		log.Println("Deprecated Number:", deprecatedNumber)
		log.Println("Total Entry Number:", totalEntryNumber)
		// err = binary.Read(file, binary.LittleEndian, &deprecatedNumber)
		// if err != nil {
		// 	return nil, err
		// }
		// defer file.Close()
	}

	// open all memtables
	memtables, err := openAllMemtables(options)
	if err != nil {
		return nil, err
	}

	// open index
	index, err := openIndex(indexOptions{
		indexType:       options.IndexType,
		dirPath:         options.DirPath,
		partitionNum:    options.PartitionNum,
		keyHashFunction: options.KeyHashFunction,
	})
	if err != nil {
		return nil, err
	}

	// open value log
	vlog, err := openValueLog(valueLogOptions{
		dirPath:               options.DirPath,
		segmentSize:           options.ValueLogFileSize,
		blockCache:            options.BlockCache,
		partitionNum:          uint32(options.PartitionNum),
		hashKeyFunction:       options.KeyHashFunction,
		compactBatchCount:     options.CompactBatchCount,
		deprecatedtableNumber: deprecatedNumber,
		totalNumber:           totalEntryNumber,
	})
	if err != nil {
		return nil, err
	}

	diskIO := new(DiskIO)
	diskIO.targetPath = options.DirPath
	diskIO.samplingInterval = options.diskIOSamplingInterval
	diskIO.busyRate = options.diskIOBusyRate

	db := &DB{
		activeMem:        memtables[len(memtables)-1],
		immuMems:         memtables[:len(memtables)-1],
		index:            index,
		vlog:             vlog,
		fileLock:         fileLock,
		flushChan:        make(chan *memtable, options.MemtableNums-1),
		closeflushChan:   make(chan struct{}),
		closeCompactChan: make(chan struct{}),
		compactChan:      make(chan deprecatedState),
		diskIO:           diskIO,
		options:          options,
		batchPool:        sync.Pool{New: makeBatch},
	}

	// if there are some immutable memtables when opening the database, flush them to disk
	if len(db.immuMems) > 0 {
		for _, table := range db.immuMems {
			db.flushMemtable(table)
		}
	}

	// start flush memtables goroutine asynchronously,
	// memtables with new coming writes will be flushed to disk if the active memtable is full.
	go db.listenMemtableFlush()

	// start disk IO monitoring,
	// blocking low threshold compact operations when busy.
	if runtime.GOOS == "linux" {
		go db.listenDiskIOState()
	}

	// start autoCompact goroutine asynchronously,
	// listen deprecatedtable state, and compact automatically.
	if options.autoCompact {
		go db.listenAutoCompact()
	}

	return db, nil
}

// Close the database, close all data files and release file lock.
// Set the closed flag to true.
// The DB instance cannot be used after closing.
func (db *DB) Close() error {
	close(db.flushChan)
	<-db.closeflushChan
	if db.options.autoCompact {
		close(db.compactChan)
		<-db.closeCompactChan
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	// close all memtables
	for _, table := range db.immuMems {
		if err := table.close(); err != nil {
			return err
		}
	}
	if err := db.activeMem.close(); err != nil {
		return err
	}
	// close index
	if err := db.index.Close(); err != nil {
		return err
	}

	// persist deprecated number and total entry number
	deprecatedMetaPath := filepath.Join(db.options.DirPath, deprecatedMetaName)
	file, err := os.OpenFile(deprecatedMetaPath, os.O_RDWR|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	deprecatedNumber := db.vlog.deprecatedNumber
	err = binary.Write(file, binary.LittleEndian, &deprecatedNumber)
	if err != nil {
		return err
	}
	log.Println("store deprecatedNumber:", deprecatedNumber)
	totalEntryNumberNumber := db.vlog.totalNumber
	err = binary.Write(file, binary.LittleEndian, &totalEntryNumberNumber)
	if err != nil {
		return err
	}
	log.Println("store totalNumber:", totalEntryNumberNumber)
	defer file.Close()

	// close value log
	if err = db.vlog.close(); err != nil {
		return err
	}
	// release file lock
	if err = db.fileLock.Unlock(); err != nil {
		return err
	}

	db.closed = true
	return nil
}

// Sync all data files to the underlying storage.
func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// sync all wal of memtables
	for _, table := range db.immuMems {
		if err := table.sync(); err != nil {
			return err
		}
	}
	if err := db.activeMem.sync(); err != nil {
		return err
	}
	// sync index
	if err := db.index.Sync(); err != nil {
		return err
	}
	// sync value log
	if err := db.vlog.sync(); err != nil {
		return err
	}

	return nil
}

// Put put with defaultWriteOptions.
func (db *DB) Put(key []byte, value []byte) error {
	return db.PutWithOptions(key, value, DefaultWriteOptions)
}

// PutWithOptions a key-value pair into the database.
// Actually, it will open a new batch and commit it.
// You can think the batch has only one Put operation.
func (db *DB) PutWithOptions(key []byte, value []byte, options WriteOptions) error {
	batch, ok := db.batchPool.Get().(*Batch)
	if !ok {
		panic("batchPoll.Get failed")
	}
	batch.options.WriteOptions = options
	defer func() {
		batch.reset()
		db.batchPool.Put(batch)
	}()
	// This is a single put operation, we can set Sync to false.
	// Because the data will be written to the WAL,
	// and the WAL file will be synced to disk according to the DB options.
	batch.init(false, false, false, db).withPendingWrites()
	if err := batch.Put(key, value); err != nil {
		batch.unlock()
		return err
	}
	return batch.Commit()
}

// Get the value of the specified key from the database.
// Actually, it will open a new batch and commit it.
// You can think the batch has only one Get operation.
func (db *DB) Get(key []byte) ([]byte, error) {
	batch, ok := db.batchPool.Get().(*Batch)
	if !ok {
		panic("batchPoll.Get failed")
	}
	batch.init(true, false, true, db)
	defer func() {
		_ = batch.Commit()
		batch.reset()
		db.batchPool.Put(batch)
	}()
	return batch.Get(key)
}

// Delete delete with defaultWriteOptions.
func (db *DB) Delete(key []byte) error {
	return db.DeleteWithOptions(key, DefaultWriteOptions)
}

// DeleteWithOptions the specified key from the database.
// Actually, it will open a new batch and commit it.
// You can think the batch has only one Delete operation.
func (db *DB) DeleteWithOptions(key []byte, options WriteOptions) error {
	batch, ok := db.batchPool.Get().(*Batch)
	if !ok {
		panic("batchPoll.Get failed")
	}
	batch.options.WriteOptions = options
	defer func() {
		batch.reset()
		db.batchPool.Put(batch)
	}()
	// This is a single delete operation, we can set Sync to false.
	// Because the data will be written to the WAL,
	// and the WAL file will be synced to disk according to the DB options.
	batch.init(false, false, false, db).withPendingWrites()
	if err := batch.Delete(key); err != nil {
		batch.unlock()
		return err
	}
	return batch.Commit()
}

// Exist checks if the specified key exists in the database.
// Actually, it will open a new batch and commit it.
// You can think the batch has only one Exist operation.
func (db *DB) Exist(key []byte) (bool, error) {
	batch, ok := db.batchPool.Get().(*Batch)
	if !ok {
		panic("batchPoll.Get failed")
	}
	batch.init(true, false, true, db)
	defer func() {
		_ = batch.Commit()
		batch.reset()
		db.batchPool.Put(batch)
	}()
	return batch.Exist(key)
}

// validateOptions validates the given options.
func validateOptions(options *Options) error {
	if options.DirPath == "" {
		return ErrDBDirectoryISEmpty
	}
	if options.MemtableSize <= 0 {
		options.MemtableSize = DefaultOptions.MemtableSize
	}
	if options.MemtableNums <= 0 {
		options.MemtableNums = DefaultOptions.MemtableNums
	}
	if options.PartitionNum <= 0 {
		options.PartitionNum = DefaultOptions.PartitionNum
	}
	if options.ValueLogFileSize <= 0 {
		options.ValueLogFileSize = DefaultOptions.ValueLogFileSize
	}
	// assure ValueLogFileSize >= MemtableSize
	if options.ValueLogFileSize < int64(options.MemtableSize) {
		options.ValueLogFileSize = int64(options.MemtableSize)
	}
	return nil
}

// get all memtables, including active memtable and immutable memtables.
// must be called with db.mu held.
func (db *DB) getMemTables() []*memtable {
	var tables []*memtable
	tables = append(tables, db.activeMem)

	last := len(db.immuMems) - 1
	for i := range db.immuMems {
		tables = append(tables, db.immuMems[last-i])
	}

	return tables
}

// waitMemtableSpace waits for space in the memtable.
// If the active memtable is full, it will be flushed to disk by the background goroutine.
// But if the flush speed is slower than the write speed, there may be no space in the memtable.
// So the write operation will wait for space in the memtable, and the timeout is specified by WaitMemSpaceTimeout.
func (db *DB) waitMemtableSpace() error {
	if !db.activeMem.isFull() {
		return nil
	}

	timer := time.NewTimer(db.options.WaitMemSpaceTimeout)
	defer timer.Stop()
	select {
	case db.flushChan <- db.activeMem:
		db.immuMems = append(db.immuMems, db.activeMem)
		options := db.activeMem.options
		options.tableID++
		// open a new memtable for writing
		table, err := openMemtable(options)
		if err != nil {
			return err
		}
		db.activeMem = table
	case <-timer.C:
		return ErrWaitMemtableSpaceTimeOut
	}

	return nil
}

// flushMemtable flushes the specified memtable to disk.
// Following steps will be done:
// 1. Iterate all records in memtable, divide them into deleted keys and log records.
// 2. Write the log records to value log, get the positions of keys.
// 3. Add old uuid, write all keys and positions to index.
// 4. Add deleted uuid, and delete the deleted keys from index.
// 5. Delete the wal.
//
//nolint:funlen
func (db *DB) flushMemtable(table *memtable) {
	{
		db.flushLock.Lock()
		defer db.flushLock.Unlock()
		sklIter := table.skl.NewIterator()
		var deletedKeys [][]byte
		var logRecords []*ValueLogRecord
	
		// iterate all records in memtable, divide them into deleted keys and log records
		// for every log record, we generate uuid.
		for sklIter.SeekToFirst(); sklIter.Valid(); sklIter.Next() {
			key, valueStruct := y.ParseKey(sklIter.Key()), sklIter.Value()
			if valueStruct.Meta == LogRecordDeleted {
				deletedKeys = append(deletedKeys, key)
			} else {
				logRecord := ValueLogRecord{key: key, value: valueStruct.Value, uid: uuid.New()}
				logRecords = append(logRecords, &logRecord)
			}
		}
		_ = sklIter.Close()
	
		// write to value log, get the positions of keys
		keyPos, err := db.vlog.writeBatch(logRecords)
		if err != nil {
			log.Println("vlog writeBatch failed:", err)
			return
		}
	
		// sync the value log
		if err = db.vlog.sync(); err != nil {
			log.Println("vlog sync failed:", err)
			return
		}
	
		// Add old key uuid into deprecatedtable, write all keys and positions to index.
		var putMatchKeys []diskhash.MatchKeyFunc
		if db.options.IndexType == Hash && len(keyPos) > 0 {
			putMatchKeys = make([]diskhash.MatchKeyFunc, len(keyPos))
			for i := range putMatchKeys {
				putMatchKeys[i] = MatchKeyFunc(db, keyPos[i].key, nil, nil)
			}
		}
	
		// Write all keys and positions to index.
		oldKeyPostions, err := db.index.PutBatch(keyPos, putMatchKeys...)
		if err != nil {
			log.Println("index PutBatch failed:", err)
			return
		}
	
		// Add old key uuid into deprecatedtable
		for _, oldKeyPostion := range oldKeyPostions {
			db.vlog.setDeprecated(oldKeyPostion.partition, oldKeyPostion.uid)
		}
	
		// Add deleted key uuid into deprecatedtable, and delete the deleted keys from index.
		var deleteMatchKeys []diskhash.MatchKeyFunc
		if db.options.IndexType == Hash && len(deletedKeys) > 0 {
			deleteMatchKeys = make([]diskhash.MatchKeyFunc, len(deletedKeys))
			for i := range deleteMatchKeys {
				deleteMatchKeys[i] = MatchKeyFunc(db, deletedKeys[i], nil, nil)
			}
		}
	
		// delete the deleted keys from index
		if oldKeyPostions, err = db.index.DeleteBatch(deletedKeys, deleteMatchKeys...); err != nil {
			log.Println("index DeleteBatch failed:", err)
			return
		}
	
		// uuid into deprecatedtable
		for _, oldKeyPostion := range oldKeyPostions {
			db.vlog.setDeprecated(oldKeyPostion.partition, oldKeyPostion.uid)
		}
		// sync the index
		if err = db.index.Sync(); err != nil {
			log.Println("index sync failed:", err)
			return
		}
	
		// delete the wal
		if err = table.deleteWAl(); err != nil {
			log.Println("delete wal failed:", err)
			return
		}
	
		// delete old memtable kept in memory
		db.mu.Lock()
		if table == db.activeMem {
			options := db.activeMem.options
			options.tableID++
			// open a new memtable for writing
			table, err = openMemtable(options)
			if err != nil {
				panic("flush activate memtable wrong")
			}
			db.activeMem = table
		} else {
			if len(db.immuMems) == 1 {
				db.immuMems = db.immuMems[:0]
			} else {
				db.immuMems = db.immuMems[1:]
			}
		}
		db.mu.Unlock()
	}

	if db.options.autoCompact {
		// check deprecatedtable size
		lowerThreshold := uint32((float32)(db.vlog.totalNumber) * db.options.deprecatedtableLowerRate)
		upperThreshold := uint32((float32)(db.vlog.totalNumber) * db.options.deprecatedtableUpperRate)
		log.Println("[data in flush]", "deprecatedNumber:", db.vlog.deprecatedNumber,
			"LowerThreshold by rate:", lowerThreshold,
			"UpperThreshold by rate:", upperThreshold)
		if db.vlog.deprecatedNumber >= upperThreshold {
			db.compactChan <- deprecatedState{
				thresholdState: ThresholdState(ArriveUpperThreshold),
			}
		} else if db.vlog.deprecatedNumber > lowerThreshold {
			db.compactChan <- deprecatedState{
				thresholdState: ThresholdState(ArriveLowerThreshold),
			}
		}
	}
}

func (db *DB) listenMemtableFlush() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	for {
		select {
		case table, ok := <-db.flushChan:
			if ok {
				db.flushMemtable(table)
			} else {
				db.closeflushChan <- struct{}{}
				return
			}
		case <-sig:
			return
		}
	}
}

// listenAutoComapct is an automated, more fine-grained approach that does not block Bptree.
// it dynamically detects the redundancy of each shard and decides
// determine whether to do compact based on the current IO state.
//
//nolint:gocognit
func (db *DB) listenAutoCompact() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	firstCompact := true
	for {
		select {
		case state, ok := <-db.compactChan:
			var err error
			err = nil
			//nolint:nestif // It requires multiple nested conditions for different thresholds and error judgments.
			if ok {
				if state.thresholdState == ThresholdState(ArriveUpperThreshold) {
					// compact right now
					log.Println("ArriveUpperThreshold")
					if firstCompact {
						firstCompact = false
						err = db.Compact()
					} else {
						err = db.CompactWithDeprecatedtable()
					}
				} else if state.thresholdState == ThresholdState(ArriveLowerThreshold) {
					// determine whether to do compact based on the current IO state
					log.Println("ArriveLowerThreshold")
					var free bool
					free, err = db.diskIO.IsFree()
					if err != nil {
						panic(err)
					}
					if free {
						if firstCompact {
							firstCompact = false
							err = db.Compact()
						} else {
							err = db.CompactWithDeprecatedtable()
						}
					} else {
						log.Println("IO Busy now.")
					}
				}
				if err != nil {
					panic(err)
				}
				for len(db.compactChan) > 0 {
					// discard squeezed messages
					<-db.compactChan
				}
			} else {
				db.closeCompactChan <- struct{}{}
				return
			}
		case <-sig:
			return
		}
	}
}

func (db *DB) listenDiskIOState() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	for {
		select {
		case <-sig:
			return
		default:
			err := db.diskIO.Monitor()
			if err != nil {
				panic(err)
			}
		}
	}
}

// Compact will iterate all values in vlog, and write the valid values to a new vlog file.
// Then replace the old vlog file with the new one, and delete the old one.
//
//nolint:gocognit,funlen
func (db *DB) Compact() error {
	log.Println("696:db.flushLock.Lock")
	db.flushLock.Lock()
	defer db.flushLock.Unlock()
	log.Println("[Compact data]")
	openVlogFile := func(part int, ext string) *wal.WAL {
		walFile, err := wal.Open(wal.Options{
			DirPath:        db.vlog.options.dirPath,
			SegmentSize:    db.vlog.options.segmentSize,
			SegmentFileExt: fmt.Sprintf(ext, part),
			BlockCache:     db.vlog.options.blockCache,
			Sync:           false, // we will sync manually
			BytesPerSync:   0,     // the same as Sync
		})
		if err != nil {
			_ = walFile.Delete()
			panic(err)
		}
		return walFile
	}

	g, _ := errgroup.WithContext(context.Background())
	for i := 0; i < int(db.vlog.options.partitionNum); i++ {
		part := i
		g.Go(func() error {
			var bptreeTime time.Duration
			var readerTime time.Duration
			var rewriteTime time.Duration
			var allTime time.Duration
			newVlogFile := openVlogFile(part, tempValueLogFileExt)

			validRecords := make([]*ValueLogRecord, 0, db.vlog.options.compactBatchCount)
			reader := db.vlog.walFiles[part].NewReader()
			count := 0
			allsTime := time.Now()
			// iterate all records in wal, find the valid records
			for {
				count++
				clsTime := time.Now()
				chunk, pos, err := reader.Next()
				cleTime := time.Now()
				readerTime += cleTime.Sub(clsTime)
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					_ = newVlogFile.Delete()
					return err
				}

				record := decodeValueLogRecord(chunk)
				var hashTableKeyPos *KeyPosition
				var matchKey func(diskhash.Slot) (bool, error)
				if db.options.IndexType == Hash {
					matchKey = MatchKeyFunc(db, record.key, &hashTableKeyPos, nil)
				}
				bptreesTime := time.Now()
				keyPos, err := db.index.Get(record.key, matchKey)
				bptreeeTime := time.Now()
				bptreeTime += bptreeeTime.Sub(bptreesTime)
				if err != nil {
					_ = newVlogFile.Delete()
					return err
				}

				if db.options.IndexType == Hash {
					keyPos = hashTableKeyPos
				}

				if keyPos == nil {
					continue
				}
				if keyPos.partition == uint32(part) && reflect.DeepEqual(keyPos.position, pos) {
					validRecords = append(validRecords, record)
				}
				rewsTime := time.Now()
				if count%db.vlog.options.compactBatchCount == 0 {
					err = db.rewriteValidRecords(newVlogFile, validRecords, part)
					if err != nil {
						_ = newVlogFile.Delete()
						return err
					}
					validRecords = validRecords[:0]
				}
				reweTime := time.Now()
				rewriteTime += reweTime.Sub(rewsTime)
			}

			if len(validRecords) > 0 {
				rewsTime := time.Now()
				err := db.rewriteValidRecords(newVlogFile, validRecords, part)
				reweTime := time.Now()
				rewriteTime += reweTime.Sub(rewsTime)
				if err != nil {
					_ = newVlogFile.Delete()
					return err
				}
			}

			// replace the wal with the new one.
			_ = db.vlog.walFiles[part].Delete()
			_ = newVlogFile.Close()
			if err := newVlogFile.RenameFileExt(fmt.Sprintf(valueLogFileExt, part)); err != nil {
				return err
			}
			db.vlog.walFiles[part] = openVlogFile(part, valueLogFileExt)
			alleTime := time.Now()
			// clean dpTable after compact

			db.vlog.dpTables[part].clean()

			allTime = alleTime.Sub(allsTime)
			log.Printf("(shard:%d) bptreeTime:%4v\treaderTIme:%4v\trewriteTime:%4v\tall:%4v\n",
				part, bptreeTime, readerTime, rewriteTime, allTime)
			return nil
		})
	}
	db.vlog.cleanDeprecatedTable()
	return g.Wait()
}

// Compact will iterate all values in vlog, find old values by deprecatedtable,
// and write the valid values to a new vlog file.
// Then replace the old vlog file with the new one, and delete the old one.
//
//nolint:gocognit,funlen
func (db *DB) CompactWithDeprecatedtable() error {
	log.Println("822:db.flushLock.Lock")
	db.flushLock.Lock()
	defer db.flushLock.Unlock()
	log.Println("[CompactWithDeprecatedtable data]")
	openVlogFile := func(part int, ext string) *wal.WAL {
		walFile, err := wal.Open(wal.Options{
			DirPath:        db.vlog.options.dirPath,
			SegmentSize:    db.vlog.options.segmentSize,
			SegmentFileExt: fmt.Sprintf(ext, part),
			BlockCache:     db.vlog.options.blockCache,
			Sync:           false, // we will sync manually
			BytesPerSync:   0,     // the same as Sync
		})
		if err != nil {
			_ = walFile.Delete()
			panic(err)
		}
		return walFile
	}

	g, _ := errgroup.WithContext(context.Background())
	for i := 0; i < int(db.vlog.options.partitionNum); i++ {
		part := i
		g.Go(func() error {
			var dptableTime time.Duration
			var readerTime time.Duration
			var rewriteTime time.Duration
			var allTime time.Duration
			newVlogFile := openVlogFile(part, tempValueLogFileExt)

			validRecords := make([]*ValueLogRecord, 0, db.vlog.options.compactBatchCount)
			reader := db.vlog.walFiles[part].NewReader()
			count := 0
			allsTime := time.Now()
			// iterate all records in wal, find the valid records
			for {
				count++
				clsTime := time.Now()
				chunk, pos, err := reader.Next()
				cleTime := time.Now()
				readerTime += cleTime.Sub(clsTime)
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					_ = newVlogFile.Delete()
					return err
				}

				record := decodeValueLogRecord(chunk)
				startTime := time.Now()
				if !db.vlog.isDeprecated(part, record.uid) {
					// not find old uuid in dptable, we add it to validRecords.
					validRecords = append(validRecords, record)
				}
				endTime := time.Now()
				dptableTime += endTime.Sub(startTime)
				if db.options.IndexType == Hash {
					var hashTableKeyPos *KeyPosition
					// var matchKey func(diskhash.Slot) (bool, error)
					matchKey := MatchKeyFunc(db, record.key, &hashTableKeyPos, nil)
					var keyPos *KeyPosition
					keyPos, err = db.index.Get(record.key, matchKey)
					if err != nil {
						_ = newVlogFile.Delete()
						return err
					}

					if db.options.IndexType == Hash {
						keyPos = hashTableKeyPos
					}

					if keyPos == nil {
						continue
					}
					if keyPos.partition == uint32(part) && reflect.DeepEqual(keyPos.position, pos) {
						validRecords = append(validRecords, record)
					}
				}

				if count%db.vlog.options.compactBatchCount == 0 {
					rewsTime := time.Now()
					err = db.rewriteValidRecords(newVlogFile, validRecords, part)
					reweTime := time.Now()
					rewriteTime += reweTime.Sub(rewsTime)

					if err != nil {
						_ = newVlogFile.Delete()
						return err
					}
					validRecords = validRecords[:0]
				}
			}
			if len(validRecords) > 0 {
				err := db.rewriteValidRecords(newVlogFile, validRecords, part)
				if err != nil {
					_ = newVlogFile.Delete()
					return err
				}
			}

			// replace the wal with the new one.
			_ = db.vlog.walFiles[part].Delete()
			_ = newVlogFile.Close()
			if err := newVlogFile.RenameFileExt(fmt.Sprintf(valueLogFileExt, part)); err != nil {
				return err
			}
			db.vlog.walFiles[part] = openVlogFile(part, valueLogFileExt)
			alleTime := time.Now()
			allTime = alleTime.Sub(allsTime)
			log.Printf("(shard:%d) dptableTime:%4v\treader:%4v\trewriteTime:%4v\tall:%4v\n",
				part, dptableTime, readerTime, rewriteTime, allTime)
			return nil
		})
	}

	err := g.Wait()
	db.vlog.cleanDeprecatedTable()
	return err
}

func (db *DB) rewriteValidRecords(walFile *wal.WAL, validRecords []*ValueLogRecord, part int) error {
	for _, record := range validRecords {
		walFile.PendingWrites(encodeValueLogRecord(record))
	}

	walChunkPositions, err := walFile.WriteAll()
	if err != nil {
		return err
	}

	positions := make([]*KeyPosition, len(walChunkPositions))
	for i, walChunkPosition := range walChunkPositions {
		positions[i] = &KeyPosition{
			key:       validRecords[i].key,
			partition: uint32(part),
			position:  walChunkPosition,
		}
	}
	matchKeys := make([]diskhash.MatchKeyFunc, len(positions))
	if db.options.IndexType == Hash {
		for i := range matchKeys {
			matchKeys[i] = MatchKeyFunc(db, positions[i].key, nil, nil)
		}
	}
	_, err = db.index.PutBatch(positions, matchKeys...)
	return err
}
