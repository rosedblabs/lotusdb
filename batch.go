package lotusdb

import (
	"github.com/bwmarrin/snowflake"
	"sync"
)

type Batch struct {
	db            *DB
	pendingWrites map[string]*LogRecord
	options       BatchOptions
	mu            sync.RWMutex
	committed     bool
	batchId       *snowflake.Node
}

func (b *Batch) Put(key []byte, value []byte) error {
	return nil
}

func (b *Batch) Get(key []byte, value []byte) ([]byte, error) {
	return nil, nil
}

func (b *Batch) Delete(key []byte) error {
	return nil
}

func (b *Batch) Exist(key []byte) (bool, error) {
	return false, nil
}

func (b *Batch) Commit(key []byte) error {
	return nil
}
