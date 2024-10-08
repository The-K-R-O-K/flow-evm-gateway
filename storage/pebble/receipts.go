package pebble

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/onflow/go-ethereum/common"
	gethTypes "github.com/onflow/go-ethereum/core/types"
	"github.com/onflow/go-ethereum/rlp"

	"github.com/onflow/flow-evm-gateway/models"
	errs "github.com/onflow/flow-evm-gateway/models/errors"
	"github.com/onflow/flow-evm-gateway/storage"
)

var _ storage.ReceiptIndexer = &Receipts{}

type Receipts struct {
	store *Storage
	mux   sync.RWMutex
}

func NewReceipts(store *Storage) *Receipts {
	return &Receipts{
		store: store,
		mux:   sync.RWMutex{},
	}
}

// Store receipt in the index.
//
// Storing receipt will create multiple indexes, each receipt has a transaction ID, and a block height.
// We create following mappings:
// - receipt transaction ID => block height bytes
// - receipt block height => list of encoded receipts (1+ per block)
// - receipt block height => list of bloom filters (1+ per block)
func (r *Receipts) Store(receipts []*models.StorageReceipt, batch *pebble.Batch) error {
	r.mux.Lock()
	defer r.mux.Unlock()

	var blooms []*gethTypes.Bloom
	var height uint64

	for _, receipt := range receipts {
		h := receipt.BlockNumber.Uint64()
		if height != 0 && h != height { // extra safety check
			return fmt.Errorf("can't store receipts for multiple heights")
		}

		height = h
		blooms = append(blooms, &receipt.Bloom)

		if err := r.store.set(
			receiptTxIDToHeightKey,
			receipt.TxHash.Bytes(),
			uint64Bytes(height),
			batch,
		); err != nil {
			return fmt.Errorf(
				"failed to store receipt tx ID: %s to height: %d mapping, with: %w",
				receipt.TxHash,
				height,
				err,
			)
		}
	}

	receiptBytes, err := rlp.EncodeToBytes(receipts)
	if err != nil {
		return err
	}

	heightBytes := uint64Bytes(height)

	if err := r.store.set(receiptHeightKey, heightBytes, receiptBytes, batch); err != nil {
		return fmt.Errorf("failed to store receipt height: %d, with: %w", height, err)
	}

	bloomBytes, err := rlp.EncodeToBytes(blooms)
	if err != nil {
		return fmt.Errorf("failed to encode blooms for height: %d, with: %w", height, err)
	}

	if err := r.store.set(bloomHeightKey, heightBytes, bloomBytes, batch); err != nil {
		return fmt.Errorf("failed to store blooms at height: %d, with: %w", height, err)
	}

	return nil
}

func (r *Receipts) GetByTransactionID(ID common.Hash) (*models.StorageReceipt, error) {
	r.mux.RLock()
	defer r.mux.RUnlock()

	height, err := r.store.get(receiptTxIDToHeightKey, ID.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to get receipt by tx ID: %s, with: %w", ID, err)
	}

	receipts, err := r.getByBlockHeight(height, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get receipt by height: %d, with: %w", height, err)
	}

	for _, rcp := range receipts {
		if rcp.TxHash.Cmp(ID) == 0 {
			return rcp, nil
		}
	}

	return nil, errs.ErrEntityNotFound
}

func (r *Receipts) GetByBlockHeight(height uint64) ([]*models.StorageReceipt, error) {
	r.mux.RLock()
	defer r.mux.RUnlock()

	return r.getByBlockHeight(uint64Bytes(height), nil)
}

func (r *Receipts) getByBlockHeight(height []byte, batch *pebble.Batch) ([]*models.StorageReceipt, error) {
	var val []byte
	var err error

	if batch != nil {
		val, err = r.store.batchGet(batch, receiptHeightKey, height)
	} else {
		val, err = r.store.get(receiptHeightKey, height)
	}
	if err != nil {
		return nil, err
	}

	var receipts []*models.StorageReceipt
	if err = rlp.DecodeBytes(val, &receipts); err != nil {
		return nil, fmt.Errorf(
			"failed to RLP-decode block receipts [%x] at height: %d, with: %w",
			val,
			height,
			err,
		)
	}

	for _, rcp := range receipts {
		// dynamically populate the values since they are not stored to save space
		for i, l := range rcp.Logs {
			l.BlockHash = rcp.BlockHash
			l.BlockNumber = rcp.BlockNumber.Uint64()
			l.TxHash = rcp.TxHash
			l.TxIndex = rcp.TransactionIndex
			l.Index = uint(i)
		}
	}

	return receipts, nil
}

func (r *Receipts) BloomsForBlockRange(start, end uint64) ([]*models.BloomsHeight, error) {
	r.mux.RLock()
	defer r.mux.RUnlock()

	if start > end {
		return nil, fmt.Errorf(
			"%w: start value %d is bigger than end value %d",
			errs.ErrInvalidBlockRange,
			start,
			end,
		)
	}

	// make sure the first and last height are within indexed values
	last, err := r.getLast()
	if err != nil {
		return nil, fmt.Errorf("failed getting first and last height: %w", err)
	}

	if start > last {
		return nil, fmt.Errorf(
			"%w: start value %d is not within the indexed range of [0 - %d]",
			errs.ErrInvalidBlockRange,
			start,
			last,
		)
	}

	if end > last {
		return nil, fmt.Errorf(
			"%w: end value %d is not within the indexed range of [0 - %d]",
			errs.ErrInvalidBlockRange,
			end,
			last,
		)
	}

	// we increase end by 1 since the range is exclusive at the upper boundary: [start, end)
	endInclusive := end + 1
	iterator, err := r.store.db.NewIter(&pebble.IterOptions{
		LowerBound: makePrefix(bloomHeightKey, uint64Bytes(start)),        // inclusive
		UpperBound: makePrefix(bloomHeightKey, uint64Bytes(endInclusive)), // exclusive
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		err := iterator.Close()
		if err != nil {
			r.store.log.Error().Err(err).Msg("failed to close receipt iterator")
		}
	}()

	bloomsHeights := make([]*models.BloomsHeight, 0)

	for iterator.First(); iterator.Valid(); iterator.Next() {
		val, err := iterator.ValueAndErr()
		if err != nil {
			return nil, err
		}

		height := stripPrefix(iterator.Key())

		var bloomsHeight []*gethTypes.Bloom
		if err := rlp.DecodeBytes(val, &bloomsHeight); err != nil {
			return nil, fmt.Errorf(
				"failed to RLP-decode blooms for range [%x] at height: %d, with: %w",
				val,
				height,
				err,
			)
		}

		bloomsHeights = append(bloomsHeights, &models.BloomsHeight{
			Blooms: bloomsHeight,
			Height: binary.BigEndian.Uint64(height),
		})
	}

	return bloomsHeights, nil
}

func (r *Receipts) getLast() (uint64, error) {
	l, err := r.store.get(latestEVMHeightKey)
	if err != nil {
		return 0, fmt.Errorf("failed getting latest EVM height: %w", err)
	}

	return binary.BigEndian.Uint64(l), nil
}
