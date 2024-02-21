package pebble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/onflow/flow-evm-gateway/storage"
	errs "github.com/onflow/flow-evm-gateway/storage/errors"
	"github.com/onflow/flow-go/fvm/evm/types"
)

var _ storage.BlockIndexer = &Blocks{}

type BlockOption func(block *Blocks) error

// WithInitHeight sets the first and last height to the provided value,
// this should be used to initialize an empty database, if the first and last
// heights are already set an error will be returned.
func WithInitHeight(height uint64) BlockOption {
	return func(block *Blocks) error {
		return block.storeInitHeight(height)
	}
}

type Blocks struct {
	store *Storage
	mux   sync.RWMutex
	// todo LRU caching with size limit
	heightCache map[byte]uint64
}

func NewBlocks(store *Storage, opts ...BlockOption) (*Blocks, error) {
	blk := &Blocks{
		store:       store,
		mux:         sync.RWMutex{},
		heightCache: make(map[byte]uint64),
	}

	for _, opt := range opts {
		if err := opt(blk); err != nil {
			return nil, err
		}
	}

	return blk, nil
}

func (b *Blocks) Store(cadenceHeight uint64, block *types.Block) error {
	b.mux.Lock()
	defer b.mux.Unlock()

	val, err := block.ToBytes()
	if err != nil {
		return err
	}

	id, err := block.Hash()
	if err != nil {
		return err
	}

	// todo batch operations
	evmHeight := uint64Bytes(block.Height)
	if err := b.store.set(blockHeightKey, evmHeight, val); err != nil {
		return err
	}

	// todo check if what is more often used block by id or block by height and fix accordingly if needed
	if err := b.store.set(blockIDToHeightKey, id.Bytes(), evmHeight); err != nil {
		return err
	}

	if err := b.store.set(evmHeightToCadenceHeightKey, evmHeight, uint64Bytes(cadenceHeight)); err != nil {
		return err
	}

	return b.setLastHeight(block.Height)
}

func (b *Blocks) GetByHeight(height uint64) (*types.Block, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	first, err := b.FirstEVMHeight()
	if err != nil {
		return nil, err
	}

	last, err := b.LatestEVMHeight()
	if err != nil {
		return nil, err
	}

	// check if the requested height is within the known range
	if height < first || height > last {
		return nil, errs.NotFound
	}

	return b.getBlock(blockHeightKey, uint64Bytes(height))
}

func (b *Blocks) GetByID(ID common.Hash) (*types.Block, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	height, err := b.store.get(blockIDToHeightKey, ID.Bytes())
	if err != nil {
		return nil, err
	}

	return b.getBlock(blockHeightKey, height)
}

func (b *Blocks) LatestEVMHeight() (uint64, error) {
	return b.getHeight(latestEVMHeightKey)
}

func (b *Blocks) FirstEVMHeight() (uint64, error) {
	return b.getHeight(firstEVMHeightKey)
}

func (b *Blocks) LatestCadenceHeight() (uint64, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	latestEVM, err := b.getHeight(latestEVMHeightKey)
	if err != nil {
		return 0, err
	}

	return b.getCadenceHeight(latestEVM)
}

func (b *Blocks) getCadenceHeight(evmHeight uint64) (uint64, error) {
	val, err := b.store.get(evmHeightToCadenceHeightKey, uint64Bytes(evmHeight))
	if err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint64(val), nil
}

func (b *Blocks) GetCadenceHeight(evmHeight uint64) (uint64, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()
	return b.getCadenceHeight(evmHeight)
}

func (b *Blocks) getBlock(keyCode byte, key []byte) (*types.Block, error) {
	data, err := b.store.get(keyCode, key)
	if err != nil {
		return nil, err
	}

	return types.NewBlockFromBytes(data)
}

func (b *Blocks) setLastHeight(height uint64) error {
	err := b.store.set(latestEVMHeightKey, nil, uint64Bytes(height))
	if err != nil {
		return err
	}
	// update cache
	b.heightCache[latestEVMHeightKey] = height
	return nil
}

func (b *Blocks) getHeight(keyCode byte) (uint64, error) {
	b.mux.RLock()
	defer b.mux.RUnlock()

	if b.heightCache[keyCode] != 0 {
		return b.heightCache[keyCode], nil
	}

	val, err := b.store.get(keyCode)
	if err != nil {
		if errors.Is(err, errs.NotFound) {
			return 0, errs.NotInitialized
		}
		return 0, fmt.Errorf("failed to get height: %w", err)
	}

	h := binary.BigEndian.Uint64(val)
	b.heightCache[keyCode] = h
	return h, nil
}

func (b *Blocks) storeInitHeight(height uint64) error {
	_, err := b.store.get(firstEVMHeightKey)
	if err != nil && !errors.Is(err, errs.NotFound) {
		return fmt.Errorf("existing first height can not be overwritten")
	}
	_, err = b.store.get(latestEVMHeightKey)
	if err != nil && !errors.Is(err, errs.NotFound) {
		return fmt.Errorf("existing latest height can not be overwritten")
	}

	// todo batch
	if err := b.store.set(firstEVMHeightKey, nil, uint64Bytes(height)); err != nil {
		return err
	}

	return b.store.set(latestEVMHeightKey, nil, uint64Bytes(height))
}
