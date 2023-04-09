// Copyright 2023 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package incrservice

import (
	"context"
	"encoding/hex"
	"sync"

	"github.com/matrixorigin/matrixone/pkg/common/log"
	"github.com/matrixorigin/matrixone/pkg/common/stopper"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/pb/txn"
	"github.com/matrixorigin/matrixone/pkg/txn/client"
	"go.uber.org/zap"
)

type service struct {
	logger    *log.MOLogger
	cfg       Config
	store     IncrValueStore
	allocator valueAllocator
	deleteC   chan uint64
	stopper   *stopper.Stopper

	mu struct {
		sync.Mutex
		closed  bool
		tables  map[uint64]incrTableCache
		creates map[string][]incrTableCache
		deletes map[string][]uint64
	}
}

func NewIncrService(
	store IncrValueStore,
	cfg Config) AutoIncrementService {
	logger := getLogger()
	cfg.adjust()
	s := &service{
		logger:    logger,
		cfg:       cfg,
		store:     store,
		allocator: newValueAllocator(store),
		deleteC:   make(chan uint64, 1024),
		stopper:   stopper.NewStopper("IncrService", stopper.WithLogger(logger.RawLogger())),
	}
	s.mu.tables = make(map[uint64]incrTableCache, 1024)
	s.mu.creates = make(map[string][]incrTableCache, 1024)
	s.mu.deletes = make(map[string][]uint64, 1024)
	s.stopper.RunTask(s.deleteTableCache)
	return s
}

func (s *service) Create(
	ctx context.Context,
	tableID uint64,
	cols []AutoColumn,
	txnOp client.TxnOperator) error {
	if txnOp == nil {
		panic("txn operator is nil")
	}
	txnOp.(client.EventableTxnOperator).
		AppendEventCallback(
			client.ClosedEvent,
			s.txnClosed)
	if err := s.store.Create(ctx, tableID, cols); err != nil {
		s.logger.Error("create auto increment cache failed",
			zap.Uint64("table-id", tableID),
			zap.String("txn", hex.EncodeToString(txnOp.Txn().ID)),
			zap.Error(err))
		return err
	}
	c, err := newTableCache(
		ctx,
		tableID,
		cols,
		s.cfg,
		s.allocator)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(txnOp.Txn().ID)
	s.mu.creates[key] = append(s.mu.creates[key], c)
	return s.doCreateLocked(
		tableID,
		c,
		txnOp.Txn().ID)
}

func (s *service) Reset(
	ctx context.Context,
	oldTableID,
	newTableID uint64,
	keep bool,
	txnOp client.TxnOperator) error {
	cols, err := s.store.GetCloumns(ctx, oldTableID)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}

	if !keep {
		for idx := range cols {
			cols[idx].Offset = 0
		}
	} else if c := s.getTableCache(oldTableID); c != nil {
		// reuse ids in cache
		if err := c.adjust(ctx, cols); err != nil {
			return err
		}
	}

	if err := s.Delete(ctx, oldTableID, txnOp); err != nil {
		return err
	}
	for idx := range cols {
		cols[idx].TableID = newTableID
	}
	return s.Create(ctx, newTableID, cols, txnOp)
}

func (s *service) Delete(
	ctx context.Context,
	tableID uint64,
	txnOp client.TxnOperator) error {
	if txnOp == nil {
		panic("txn operator is nil")
	}
	txnOp.(client.EventableTxnOperator).
		AppendEventCallback(
			client.ClosedEvent,
			s.txnClosed)

	s.mu.Lock()
	defer s.mu.Unlock()

	key := string(txnOp.Txn().ID)
	s.mu.deletes[key] = append(s.mu.deletes[key], tableID)
	if s.logger.Enabled(zap.DebugLevel) {
		s.logger.Debug("ready to delete auto increment table cache",
			zap.Uint64("table-id", tableID),
			zap.String("txn", hex.EncodeToString(txnOp.Txn().ID)))
	}
	return nil
}

func (s *service) InsertValues(
	ctx context.Context,
	tableID uint64,
	bat *batch.Batch) (uint64, error) {
	ts, err := s.getCommittedTableCache(
		ctx,
		tableID)
	if err != nil {
		return 0, err
	}
	return ts.insertAutoValues(ctx, tableID, bat)
}

func (s *service) CurrentValue(
	ctx context.Context,
	tableID uint64,
	col string) (uint64, error) {
	ts, err := s.getCommittedTableCache(
		ctx,
		tableID)
	if err != nil {
		return 0, err
	}
	return ts.currentValue(ctx, tableID, col)
}

func (s *service) Close() {
	s.mu.Lock()
	if s.mu.closed {
		s.mu.Unlock()
		return
	}
	s.mu.closed = true
	for _, tc := range s.mu.tables {
		if err := tc.close(); err != nil {
			panic(err)
		}
	}
	s.mu.Unlock()

	s.stopper.Stop()
	close(s.deleteC)
	s.allocator.close()
	s.store.Close()
}

func (s *service) doCreateLocked(
	tableID uint64,
	c incrTableCache,
	txnID []byte) error {
	s.mu.tables[tableID] = c
	if s.logger.Enabled(zap.DebugLevel) {
		s.logger.Debug("auto increment cache created",
			zap.Uint64("table-id", tableID),
			zap.String("txn", hex.EncodeToString(txnID)))
	}
	return nil
}

func (s *service) getCommittedTableCache(
	ctx context.Context,
	tableID uint64) (incrTableCache, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.mu.tables[tableID]
	if ok {
		return c, nil
	}

	cols, err := s.store.GetCloumns(ctx, tableID)
	if err != nil {
		return nil, err
	}
	c, err = newTableCache(
		ctx,
		tableID,
		cols,
		s.cfg,
		s.allocator)
	if err != nil {
		return nil, err
	}
	s.doCreateLocked(tableID, c, nil)
	return c, nil
}

func (s *service) txnClosed(txnMeta txn.TxnMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.handleCreatesLocked(txnMeta)
	s.handleDeletesLocked(txnMeta)
}

func (s *service) handleCreatesLocked(txnMeta txn.TxnMeta) {
	key := string(txnMeta.ID)
	tables, ok := s.mu.creates[key]
	if !ok {
		return
	}

	if txnMeta.Status != txn.TxnStatus_Committed {
		for _, c := range tables {
			if s.logger.Enabled(zap.DebugLevel) {
				s.logger.Debug("destroy creates table",
					zap.String("resean", "txn aborted"),
					zap.String("txn", hex.EncodeToString(txnMeta.ID)),
					zap.Any("table", c.table()))
			}
			s.detroyTableCache(c.table(), true)
		}
	}
	delete(s.mu.creates, key)
}

func (s *service) handleDeletesLocked(txnMeta txn.TxnMeta) {
	key := string(txnMeta.ID)
	tables, ok := s.mu.deletes[key]
	if !ok {
		return
	}

	if s.logger.Enabled(zap.DebugLevel) {
		s.logger.Debug("destroy deletes table auto increment cache",
			zap.String("resean", "txn committed"),
			zap.String("txn", hex.EncodeToString(txnMeta.ID)),
			zap.Any("tables", tables))
	}
	if txnMeta.Status == txn.TxnStatus_Committed {
		for _, id := range tables {
			s.detroyTableCache(id, false)
		}
	}
	delete(s.mu.deletes, key)
}

func (s *service) detroyTableCache(
	tableID uint64,
	mustExists bool) {
	if _, ok := s.mu.tables[tableID]; !ok && mustExists {
		panic("missing created incr table cache")
	}
	s.deleteC <- tableID
}

func (s *service) deleteTableCache(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tableID := <-s.deleteC:
			s.mu.Lock()
			if tc, ok := s.mu.tables[tableID]; ok {
				_ = tc.close()
				delete(s.mu.tables, tableID)
			}
			s.mu.Unlock()
			for i := 0; i < 2; i++ {
				if err := s.store.Delete(ctx, tableID); err == nil {
					break
				}
			}
		}
	}
}

func (s *service) getTableCache(tableID uint64) incrTableCache {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mu.tables[tableID]
}