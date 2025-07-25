package db

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

func (d *DB) DumpTasks(ctx context.Context, prefix string) (res []*Task, err error) {
	tx := d.storage.GetExecutor(ctx)

	keyIndex := []byte("tv:" + prefix)

	iter := tx.NewIterator(keyIndex, true)
	defer iter.Release()

	for iter.Next() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var task *Task
		if err := json.Unmarshal(iter.Value(), &task); err != nil {
			return nil, fmt.Errorf("failed to decode json data: %w", err)
		}

		res = append(res, task)
	}

	sort.Slice(res, func(i, j int) bool {
		return res[i].CreatedAt.After(res[j].CreatedAt)
	})

	return res, nil
}

func (d *DB) ListActiveTasks(ctx context.Context, poolName string) ([]*Task, error) {
	var result []*Task
	tx := d.storage.GetExecutor(ctx)

	keyIndex := []byte("ti:" + poolName + ":")

	iter := tx.NewIterator(keyIndex, true)
	defer iter.Release()

	now := time.Now()

	for iter.Next() {
		key := iter.Key()

		if binary.BigEndian.Uint64(key[len(keyIndex):]) > uint64(now.UnixNano()) {
			// no tasks ready to execute
			break
		}

		dataKey := iter.Value()
		if dataKey == nil {
			continue
		}

		data, err := tx.Get(dataKey)
		if err != nil {
			return nil, fmt.Errorf("failed to get task by index: %w", err)
		}

		var task *Task
		if err := json.Unmarshal(data, &task); err != nil {
			return nil, fmt.Errorf("failed to decode json data: %w", err)
		}

		// it should be removed from index when completed, but just to be sure
		if task.CompletedAt != nil {
			continue
		}

		result = append(result, task)
	}

	return result, nil
}

func (d *DB) AcquireTask(ctx context.Context, poolName string) (*Task, error) {
	var result *Task
	err := d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		keyIndex := []byte("ti:" + poolName + ":")

		iter := tx.NewIterator(keyIndex, true)
		defer iter.Release()

		now := time.Now()

		var toSkip []string
	next:
		for iter.Next() {
			key := iter.Key()

			if binary.BigEndian.Uint64(key[len(keyIndex):]) > uint64(now.UnixNano()) {
				// no tasks ready to execute
				break
			}

			q := string(key[len(keyIndex)+8:])
			for _, skip := range toSkip {
				if q == skip {
					continue next
				}
			}

			dataKey := iter.Value()

			data, err := tx.Get(dataKey)
			if err != nil {
				return fmt.Errorf("failed to get task by index: %w", err)
			}

			var task *Task
			if err := json.Unmarshal(data, &task); err != nil {
				return fmt.Errorf("failed to decode json data: %w", err)
			}

			// it should be removed from index when completed, but just to be sure
			if task.CompletedAt != nil {
				continue
			}

			if task.LockedTill != nil && task.LockedTill.After(now) {
				// locked by someone else (already in progress)
				// we skip everything in this queue to not break the order
				toSkip = append(toSkip, task.Queue)
				continue
			}

			if task.ReExecuteAfter != nil && task.ReExecuteAfter.After(now) {
				// not yet ready to retry
				toSkip = append(toSkip, task.Queue)
				continue
			}

			if task.ExecuteTill != nil && task.ExecuteTill.Before(now) {
				// task is expired, remove from index (queue)
				if err = tx.Delete(key); err != nil {
					return fmt.Errorf("failed to delete index: %w", err)
				}
				continue
			}

			result = task

			// we need to lock task to not acquire it twice when using multiple workers
			till := time.Now().Add(5 * time.Minute)
			result.LockedTill = &till

			data, err = json.Marshal(result)
			if err != nil {
				return fmt.Errorf("failed to encode json: %w", err)
			}

			if err = tx.Put(dataKey, data); err != nil {
				return fmt.Errorf("failed to put index: %w", err)
			}

			break
		}

		if err := iter.Error(); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (d *DB) CreateTask(ctx context.Context, poolName, typ, queue, id string, data any, executeAfter, executeTill *time.Time) error {
	bts, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return d.Transaction(ctx, func(ctx context.Context) error {
		after := time.Now()
		if executeAfter != nil {
			after = *executeAfter
		}

		if err = d.createTask(ctx, &Task{
			ID:           id,
			Type:         typ,
			Queue:        queue,
			Data:         bts,
			ExecuteAfter: after,
			ExecuteTill:  executeTill,
			CreatedAt:    time.Now(),
		}, poolName); err != nil {
			if errors.Is(err, ErrAlreadyExists) {
				// idempotency
				return nil
			}
			return fmt.Errorf("failed to create task: %w", err)
		}
		return nil
	})
}

func (d *DB) CompleteTask(ctx context.Context, poolName string, task *Task) error {
	if task.CompletedAt != nil {
		return nil
	}

	now := time.Now()
	task.CompletedAt = &now
	task.LockedTill = nil

	key := append([]byte("tv:"), []byte(task.ID)...)

	// we need remove it from index to not pick it up again
	keyOrderIndex := getTaskIndexKey(task, poolName)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return ErrNotFound
		}

		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		if err = tx.Delete(keyOrderIndex); err != nil {
			return fmt.Errorf("failed to put index: %w", err)
		}
		return nil
	})
}

func (d *DB) RetryTask(ctx context.Context, task *Task, reason string, retryAt time.Time) error {
	if task.CompletedAt != nil || task.LockedTill == nil {
		return nil
	}

	task.LockedTill = nil
	task.LastError = reason
	task.ReExecuteAfter = &retryAt

	key := append([]byte("tv:"), []byte(task.ID)...)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return ErrNotFound
		}

		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) createTask(ctx context.Context, task *Task, poolName string) error {
	key := append([]byte("tv:"), []byte(task.ID)...)

	// we need an index to know processing order
	keyOrderIndex := getTaskIndexKey(task, poolName)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return ErrAlreadyExists
		}

		data, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put data: %w", err)
		}
		if err = tx.Put(keyOrderIndex, key); err != nil {
			return fmt.Errorf("failed to put index: %w", err)
		}
		return nil
	})
}

func getTaskIndexKey(task *Task, poolName string) []byte {
	at := make([]byte, 8)
	binary.BigEndian.PutUint64(at, uint64(task.ExecuteAfter.UTC().UnixNano()))

	return append(append([]byte("ti:"+poolName+":"), at...), []byte(task.Queue)...)
}
