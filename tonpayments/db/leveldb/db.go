package leveldb

import (
	"context"
	"errors"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LevelDB struct {
	path string
	_db  *leveldb.DB

	mx sync.Mutex
}

type Tx struct {
	*leveldb.Snapshot
	batchWrap
}

type batchWrap struct {
	b *leveldb.Batch
}

func (b batchWrap) Put(key, value []byte, wo *opt.WriteOptions) error {
	if !wo.GetSync() {
		panic("must be sync write")
	}

	b.b.Put(key, value)
	return nil
}

func (b batchWrap) Delete(key []byte, wo *opt.WriteOptions) error {
	if !wo.GetSync() {
		panic("must be sync write")
	}

	b.b.Delete(key)
	return nil
}

func NewLevelDB(path string) (*LevelDB, bool, error) {
	isNew := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		isNew = true
	}

	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, false, err
	}

	return &LevelDB{
		path: path,
		_db:  db,
	}, isNew, nil
}

func (d *LevelDB) Close() {
	d._db.Close()
}

const txKey = "__ldbTx"

// Transaction - kinda ACID achievement using leveldb
func (d *LevelDB) Transaction(ctx context.Context, f func(ctx context.Context) error) error {
	tx, ok := ctx.Value(txKey).(*Tx)
	if ok {
		// already inside tx
		return f(ctx)
	}

	// lock gives us consistency
	d.mx.Lock()
	defer d.mx.Unlock()

	// snapshot gives us kinda reads isolation
	snap, err := d._db.GetSnapshot()
	if err != nil {
		return fmt.Errorf("failed to get db snapshot: %w", err)
	}
	defer snap.Release()

	tx = &Tx{
		batchWrap: batchWrap{new(leveldb.Batch)},
		Snapshot:  snap,
	}

	if err := f(context.WithValue(ctx, txKey, tx)); err != nil {
		return err
	}

	// batches are atomic, and durable when sync = true
	if err := d._db.Write(tx.batchWrap.b, &opt.WriteOptions{
		Sync: true,
	}); err != nil {
		return fmt.Errorf("failed to write batch to db: %w", err)
	}
	return nil
}

type executor interface {
	Put(key, value []byte, wo *opt.WriteOptions) error
	Delete(key []byte, wo *opt.WriteOptions) error
	Get(key []byte, ro *opt.ReadOptions) (value []byte, err error)
	Has(key []byte, ro *opt.ReadOptions) (ret bool, err error)
	NewIterator(slice *util.Range, ro *opt.ReadOptions) iterator.Iterator
}

type reverseIterator struct {
	iterator.Iterator
	first bool
}

func (r *reverseIterator) Next() bool {
	if r.first {
		r.first = false
		return r.Valid()
	}
	return r.Iterator.Prev()
}

type Executor struct {
	e executor
}

func (e Executor) Put(key, value []byte) error {
	return e.e.Put(key, value, &opt.WriteOptions{
		Sync: true,
	})
}

func (e Executor) Delete(key []byte) error {
	return e.e.Delete(key, &opt.WriteOptions{
		Sync: true,
	})
}

func (e Executor) Get(key []byte) (value []byte, err error) {
	value, err = e.e.Get(key, nil)
	if err != nil && errors.Is(err, leveldb.ErrNotFound) {
		return nil, db.ErrNotFound
	}
	return
}

func (e Executor) Has(key []byte) (ret bool, err error) {
	return e.e.Has(key, nil)
}

func (e Executor) NewIterator(p []byte, forward bool) db.Iterator {
	it := e.e.NewIterator(util.BytesPrefix(p), nil)

	if !forward {
		it.Last()
		return &reverseIterator{Iterator: it, first: true}
	}

	return it
}

func (d *LevelDB) GetExecutor(ctx context.Context) db.Executor {
	if tx, ok := ctx.Value(txKey).(*Tx); ok {
		return &Executor{tx}
	}
	return &Executor{d._db}
}

func (d *LevelDB) Backup() error {
	d.mx.Lock()
	defer d.mx.Unlock()

	// Close the database before starting the backup process
	err := d._db.Close()
	if err != nil {
		return fmt.Errorf("failed to close the database before backup: %w", err)
	}

	// Ensure the database is reopened after the backup
	defer func() {
		reopenedDB, reopenErr := leveldb.OpenFile(d.path, nil)
		if reopenErr != nil {
			err = fmt.Errorf("failed to reopen the database after backup: %w", reopenErr)
			return
		}
		d._db = reopenedDB
	}()

	// Proceed with the backup
	backupDir := fmt.Sprintf("%s_backup_%d", d.path, time.Now().UnixMilli())

	err = os.MkdirAll(backupDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	err = filepath.WalkDir(d.path, func(path string, dir fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("failed to access file %s: %w", path, err)
		}

		if dir.IsDir() {
			return nil
		}

		relativePath, err := filepath.Rel(d.path, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		destinationPath := filepath.Join(backupDir, relativePath)

		err = os.MkdirAll(filepath.Dir(destinationPath), 0755)
		if err != nil {
			return fmt.Errorf("failed to create directory %s: %w", filepath.Dir(destinationPath), err)
		}

		input, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open source file %s: %w", path, err)
		}
		defer input.Close()

		output, err := os.Create(destinationPath)
		if err != nil {
			return fmt.Errorf("failed to create destination file %s: %w", destinationPath, err)
		}
		defer output.Close()

		if _, err := io.Copy(output, input); err != nil {
			return fmt.Errorf("failed to copy data from %s to %s: %w", path, destinationPath, err)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to complete backup: %w", err)
	}

	return nil
}
