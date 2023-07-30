package lotusdb

import (
	"math/rand"
	"os"
	"testing"

	"github.com/lotusdblabs/lotusdb/v2/util"
	"github.com/stretchr/testify/assert"
)

// test Open function
func TestOpen(t *testing.T) {
	// Create a instance of Options struct to pass to the Open function.
	options := DefaultOptions
	options.DirPath = t.TempDir()
	defer os.RemoveAll(options.DirPath)

	// Call the Open function for testing
	db, err := Open(options)
	assert.NoError(t, err, "Open should not return an error")
	assert.NotNil(t, db, "db should not be nil")

	// Ensure that the properties of the DB are initialized correctly.
	assert.NotNil(t, db.activeMem, "DB activeMem should not be nil")
	assert.Equal(t, db.activeMem.options.memSize, options.MemtableSize, "DB activeMem options.memSize should be equal to options.MemtableSize")
	assert.Len(t, db.immuMems, 0, "DB immuMems should be empty")
	assert.NotNil(t, db.index, "DB index should not be nil")
	assert.NotNil(t, db.vlog, "DB vlog should not be nil")
	assert.NotNil(t, db.fileLock, "DB fileLock should not be nil")
	assert.NotNil(t, db.flushChan, "DB flushChan should not be nil")
}

// test Close function
func TestClose(t *testing.T) {
	// Create a instance of Options struct to pass to the Open function.
	options := DefaultOptions
	options.DirPath = t.TempDir()
	defer os.RemoveAll(options.DirPath)

	// Call the Open function for testing
	db, err := Open(options)
	assert.NoError(t, err, "Open should not return an error")
	assert.NotNil(t, db, "db should not be nil")

	// Call the Close function for testing
	err = db.Close()
	assert.NoError(t, err, "Close should not return an error")
	assert.True(t, db.closed, "db should be closed")
}

// test Sync function
func TestSync(t *testing.T) {
	// Create a instance of Options struct to pass to the Open function.
	options := DefaultOptions
	options.DirPath = t.TempDir()
	defer os.RemoveAll(options.DirPath)
	// Call the Open function for testing
	db, err := Open(options)
	assert.NoError(t, err, "Open should not return an error")
	assert.NotNil(t, db, "db should not be nil")

	// Call the Sync function for testing
	err = db.Sync()
	assert.NoError(t, err, "Sync should not return an error")
}

// test Put and Get function
func TestDB_Put_Get(t *testing.T) {
	// Create a instance of Options struct to pass to the Open function.
	options := DefaultOptions
	options.DirPath = t.TempDir()
	defer os.RemoveAll(options.DirPath)

	// Call the Open function for testing
	db, err := Open(options)
	assert.NoError(t, err, "Open should not return an error")
	assert.NotNil(t, db, "db should not be nil")

	// Return error as KeyNotFound
	_, err = db.Get([]byte("key"))
	assert.EqualError(t, err, ErrKeyNotFound.Error(), "expected key not found")

	// Call the Put function for testing
	writeOptions := &WriteOptions{
		Sync:       false,
		DisableWal: true,
	}

	k, v := []byte("Hello"), []byte("World")
	err = db.Put(k, v, writeOptions)
	assert.NoError(t, err, "Put should not return an error")

	val, err := db.Get(k)
	assert.NoError(t, err, "Get should not return an error")
	assert.Equal(t, v, val, "expected value is World")

	for i := 0; i < 100; i++ {
		err = db.Put(util.GetTestKey(rand.Int()), util.RandomValue(128), writeOptions)
		assert.NoError(t, err, "Put should not return an error")
		err = db.Put(util.GetTestKey(rand.Int()), util.RandomValue(KB), writeOptions)
		assert.NoError(t, err, "Put should not return an error")
		err = db.Put(util.GetTestKey(rand.Int()), util.RandomValue(5*KB), writeOptions)
		assert.NoError(t, err, "Put should not return an error")
	}
	// Call the Close function for testing
	err = db.Close()
	assert.NoError(t, err, "Close should not return an error")
}

// test Delete and Exist function
func TestDB_Delete_Exist(t *testing.T) {
	// Create a instance of Options struct to pass to the Open function.
	options := DefaultOptions
	options.DirPath = t.TempDir()
	defer os.RemoveAll(options.DirPath)
	// Call the Open function for testing
	db, err := Open(options)
	assert.NoError(t, err, "Open should not return an error")
	assert.NotNil(t, db, "db should not be nil")

	// Call the Put function for testing
	writeOptions := &WriteOptions{
		Sync:       true,
		DisableWal: false,
	}

	k, v := []byte("Lumia"), []byte("Qian")
	err = db.Put(k, v, writeOptions)
	assert.NoError(t, err, "Put should not return an error")

	// Call the Exist function for testing
	isExist, err := db.Exist(k)
	assert.NoError(t, err, "Exist should not return an error")
	assert.Equal(t, true, isExist, "expected isExist is true")

	// Call the Delete function for testing
	err = db.Delete(k, writeOptions)
	assert.NoError(t, err, "Delete should not return an error")

	// Call the Exist function for testing
	isExist, err = db.Exist(k)
	assert.NoError(t, err, "Exist should not return an error")
	assert.Equal(t, false, isExist, "expected isExist is false")

	// Delete an not exist key
	err = db.Delete([]byte("Hello"), writeOptions)
	assert.NoError(t, err, "Delete should not return an error")

	// Call the Close function for testing
	err = db.Close()
	assert.NoError(t, err, "Close should not return an error")
}
