package stagedsync

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/dbutils"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/log/v3"
)

type OtsApprovalsIndexCfg struct {
	db          kv.RwDB
	chainConfig *params.ChainConfig
	blockReader services.FullBlockReader
	tmpdir      string
	isEnabled   bool
}

// const buffLimit = 256 * datasize.MB

func StageOtsApprovalsIndexCfg(db kv.RwDB, chainConfig *params.ChainConfig, blockReader services.FullBlockReader, tmpdir string, isEnabled bool) OtsApprovalsIndexCfg {
	return OtsApprovalsIndexCfg{
		db:          db,
		chainConfig: chainConfig,
		blockReader: blockReader,
		tmpdir:      tmpdir,
		isEnabled:   isEnabled,
	}
}

func SpawnStageOtsApprovalsIndex(cfg OtsApprovalsIndexCfg, s *StageState, tx kv.RwTx, ctx context.Context) error {
	if !cfg.isEnabled {
		return nil
	}

	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = cfg.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	bodiesProgress, err := stages.GetStageProgress(tx, stages.Bodies)
	if err != nil {
		return fmt.Errorf("getting bodies progress: %w", err)
	}

	// start/end block are inclusive
	startBlock := s.BlockNumber + 1
	endBlock := bodiesProgress
	if startBlock > endBlock {
		return nil
	}

	// Log timer
	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()

	// Setup approvals table
	approvalsIdx, err := tx.RwCursor(kv.OtsApprovalsIndex)
	if err != nil {
		return err
	}
	defer approvalsIdx.Close()

	collector := etl.NewCollector(s.LogPrefix(), cfg.tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
	defer collector.Close()

	stopped := false
	currentBlock := startBlock
	approvalHash := common.HexToHash("0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925")
	cache := make(map[string]*rawdb.Spenders, 4_400_000)
	for ; currentBlock <= endBlock && !stopped; /*&& currentBlock < 5000000*/ currentBlock++ {
		receipts := rawdb.ReadRawReceipts(tx, currentBlock)
		if receipts == nil {
			// Ignore on purpose, it could be a pruned receipt, which would constitute an
			// error, but also an empty block, which should be the case
			continue
		}

		for _, r := range receipts {
			for _, l := range r.Logs {
				// topics: [approvalHash, owner, spender]
				if len(l.Topics) != 3 {
					continue
				}
				if l.Topics[0] != approvalHash {
					continue
				}

				// log.Info(fmt.Sprintf("[%s] Found approval", s.LogPrefix()), "block", currentBlock, "token", l.Address, "owner", l.Topics[1], "spender", l.Topics[2])

				// Locate existing approvals for token
				ownerAddr := common.BytesToAddress(l.Topics[1].Bytes())
				spenderAddr := common.BytesToAddress(l.Topics[2].Bytes())

				key := dbutils.ApprovalsIdxKey(ownerAddr, l.Address)
				currentSpenders, ok := cache[string(key)]
				if !ok {
					spender := rawdb.NewSpender(spenderAddr)
					spender.Blocks = append(spender.Blocks, currentBlock)
					spenders := rawdb.Spenders{}
					spenders.Spenders = append(spenders.Spenders, *spender)
					cache[string(key)] = &spenders
					// log.Info(fmt.Sprintf("[%s] New spender", s.LogPrefix()), "k", hexutil.Encode(key2[:]), "size", len(cache))
				} else {
					var spenderFound *rawdb.Spender
					for _, sp := range currentSpenders.Spenders {
						if sp.Spender == spenderAddr {
							spenderFound = &sp
							break
						}
					}
					if spenderFound == nil {
						// log.Info(fmt.Sprintf("[%s] New spender", s.LogPrefix()), "token", l.Address, "owner", common.BytesToAddress(l.Topics[1].Bytes()), "spender", common.BytesToAddress(l.Topics[2].Bytes()))
						spenderFound = rawdb.NewSpender(spenderAddr)
						spenderFound.Blocks = append(spenderFound.Blocks, currentBlock)
						currentSpenders.Spenders = append(currentSpenders.Spenders, *spenderFound)
					} else {
						spenderFound.Blocks = append(spenderFound.Blocks, currentBlock)
					}
				}

				// Update or save spenders
				if len(cache) >= 4_400_000 {
					if err := flushCache(cache, s.LogPrefix(), currentBlock, collector); err != nil {
						return err
					}
					cache = make(map[string]*rawdb.Spenders, 4_400_000)
				}
			}
		}

		select {
		case <-ctx.Done():
			stopped = true
		case <-logEvery.C:
			log.Info(fmt.Sprintf("[%s] Indexing approvals", s.LogPrefix()),
				"block", currentBlock)
		default:
		}
	}

	if err := flushCache(cache, s.LogPrefix(), currentBlock, collector); err != nil {
		return err
	}
	loadFunc := func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
		prev, err := table.Get(k)
		if err != nil {
			return err
		}

		existingSpenders := rawdb.Spenders{}
		if prev != nil {
			err := rlp.DecodeBytes(prev, &existingSpenders)
			if err != nil {
				return err
			}
		}

		newSpenders := rawdb.Spenders{}
		err = rlp.DecodeBytes(v, &newSpenders)
		if err != nil {
			return err
		}

		// Merge existing spenders from DB
		for _, s := range newSpenders.Spenders {
			var spenderFound *rawdb.Spender
			for _, ps := range existingSpenders.Spenders {
				if ps.Spender == s.Spender {
					spenderFound = &ps
					break
				}
			}
			if spenderFound == nil {
				existingSpenders.Spenders = append(existingSpenders.Spenders, s)
			} else {
				spenderFound.Blocks = append(spenderFound.Blocks, s.Blocks...)
			}
		}

		// Update or save spenders
		buf, err := rlp.EncodeToBytes(existingSpenders)
		if err != nil {
			return err
		}
		if err := next(k, k, buf); err != nil {
			return err
		}

		return nil
	}
	if err := collector.Load(tx, kv.OtsApprovalsIndex, loadFunc, etl.TransformArgs{Quit: ctx.Done()}); err != nil {
		return err
	}

	if currentBlock > endBlock {
		currentBlock--
	}
	if err := s.Update(tx, currentBlock); err != nil {
		return err
	}
	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func flushCache(cache map[string]*rawdb.Spenders, logPrefix string, currentBlock uint64, collector *etl.Collector) error {
	log.Info(fmt.Sprintf("[%s] Flushing spenders", logPrefix), "block", currentBlock)
	for k, v := range cache {
		buf, err := rlp.EncodeToBytes(v)
		if err != nil {
			return err
		}
		if err := collector.Collect([]byte(k), buf); err != nil {
			return err
		}
		// log.Info(fmt.Sprintf("[%s] Collected", s.LogPrefix()), "k", hexutil.Encode(k[:]), "v", hexutil.Encode(buf))
	}
	return nil
}

func UnwindOtsApprovalsIndex(u *UnwindState, cfg OtsApprovalsIndexCfg, tx kv.RwTx, ctx context.Context) error {
	if !cfg.isEnabled {
		return nil
	}

	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err := cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	// TODO: fix EROR[06-29|23:55:47.639] Staged Sync
	// err="runtime error: invalid memory address or nil pointer dereference, trace:
	// [stageloop.go:116 panic.go:844 panic.go:220 signal_unix.go:818 stages.go:83 stage.go:97 stage_ots_approvals_index.go:253
	// default_stages.go:222 sync.go:359 sync.go:203 stageloop.go:150 stageloop.go:53 asm_amd64.s:1571]"
	if err := u.Done(tx); err != nil {
		return fmt.Errorf(" reset: %w", err)
	}
	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to write db commit: %w", err)
		}
	}
	return nil
}