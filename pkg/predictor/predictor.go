package predictor

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
)

const (
	// BlockSize is the unit of prediction
	BlockSize = 4096

	// MaxCacheBlocks is the maximum number of blocks to cache for dedup
	MaxCacheBlocks = 65536 // 256 MB worth of blocks

	// HashSize is the truncated hash size for block fingerprints
	HashSize = 8 // 8 bytes = 64 bits, sufficient for dedup
)

// BlockHash is a truncated SHA-256 fingerprint of a block
type BlockHash [HashSize]byte

// BlockPredictor performs block-level deduplication and prediction.
// Instead of predicting individual symbols, it recognizes entire blocks
// that have been seen before and transmits only a short hash reference.
//
// This is fundamentally more efficient because:
// - One comparison per 4KB block vs 16,384 comparisons per block (symbol-level)
// - Cache-friendly: hash lookups in a map vs per-symbol array walks
// - Block dedup catches exact repeats instantly
type BlockPredictor struct {
	mu sync.RWMutex

	// Known blocks: hash -> block data
	cache map[BlockHash][]byte

	// LRU ordering for eviction
	order []BlockHash
	pos   int

	// Sequence predictor: tracks the last N block hashes to predict
	// sequential patterns (e.g., same sequence of blocks repeating)
	seqHistory []BlockHash
	seqPos     int
	seqLen     int
	seqMax     int

	// Stats
	TotalBlocks   uint64
	CacheHits     uint64
	SeqHits       uint64
	CacheMisses   uint64
}

func NewBlockPredictor() *BlockPredictor {
	return &BlockPredictor{
		cache:      make(map[BlockHash][]byte, MaxCacheBlocks),
		order:      make([]BlockHash, MaxCacheBlocks),
		seqHistory: make([]BlockHash, 4096),
		seqMax:     4096,
	}
}

// HashBlock computes the fingerprint of a data block
func HashBlock(data []byte) BlockHash {
	full := sha256.Sum256(data)
	var h BlockHash
	copy(h[:], full[:HashSize])
	return h
}

// Observe records a block in the cache and sequence history
func (bp *BlockPredictor) Observe(hash BlockHash, data []byte) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	// Add to cache if not already present
	if _, exists := bp.cache[hash]; !exists {
		// Evict if full
		if len(bp.cache) >= MaxCacheBlocks {
			old := bp.order[bp.pos]
			delete(bp.cache, old)
		}

		// Store a copy
		stored := make([]byte, len(data))
		copy(stored, data)
		bp.cache[hash] = stored
		bp.order[bp.pos] = hash
		bp.pos = (bp.pos + 1) % MaxCacheBlocks
	}

	// Record in sequence history
	bp.seqHistory[bp.seqPos] = hash
	bp.seqPos = (bp.seqPos + 1) % bp.seqMax
	if bp.seqLen < bp.seqMax {
		bp.seqLen++
	}
}

// PredictNext tries to predict the next block based on sequential patterns.
// Returns the predicted hash and whether a prediction was made.
func (bp *BlockPredictor) PredictNext() (BlockHash, bool) {
	bp.mu.RLock()
	defer bp.mu.RUnlock()

	if bp.seqLen < 2 {
		return BlockHash{}, false
	}

	// Look at the last block hash and search for previous occurrences
	// of the same hash in history. If found, predict the block that
	// followed it last time.
	lastPos := (bp.seqPos - 1 + bp.seqMax) % bp.seqMax
	lastHash := bp.seqHistory[lastPos]

	// Search backwards through history for a previous occurrence
	// Skip the most recent entry (that's the one we're trying to predict from)
	for i := 2; i < bp.seqLen; i++ {
		checkPos := (bp.seqPos - i + bp.seqMax) % bp.seqMax
		if bp.seqHistory[checkPos] == lastHash {
			// Found a previous occurrence — predict the block that followed
			followPos := (checkPos + 1) % bp.seqMax
			if followPos != bp.seqPos { // Don't predict from ourselves
				return bp.seqHistory[followPos], true
			}
		}
	}

	return BlockHash{}, false
}

// Lookup checks if a block hash is in the cache and returns the data
func (bp *BlockPredictor) Lookup(hash BlockHash) ([]byte, bool) {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	data, ok := bp.cache[hash]
	return data, ok
}

// Known checks if a block hash exists in the cache
func (bp *BlockPredictor) Known(hash BlockHash) bool {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	_, ok := bp.cache[hash]
	return ok
}

// PreTrainFromSeed generates synthetic block patterns for initial cache
func (bp *BlockPredictor) PreTrainFromSeed(seed [32]byte) {
	// Generate some common block patterns
	// All-zeros block (very common in storage)
	zeroBlock := make([]byte, BlockSize)
	bp.Observe(HashBlock(zeroBlock), zeroBlock)

	// All-ones block
	onesBlock := make([]byte, BlockSize)
	for i := range onesBlock {
		onesBlock[i] = 0xFF
	}
	bp.Observe(HashBlock(onesBlock), onesBlock)

	// Seed-derived pattern blocks
	for i := 0; i < 32; i++ {
		h := sha256.New()
		h.Write(seed[:])
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(i))
		h.Write(buf)
		pattern := h.Sum(nil)

		block := make([]byte, BlockSize)
		for j := 0; j < BlockSize; j++ {
			block[j] = pattern[j%len(pattern)]
		}
		bp.Observe(HashBlock(block), block)
	}
}

// Accuracy returns the prediction/dedup hit rate
func (bp *BlockPredictor) Accuracy() float64 {
	total := bp.TotalBlocks
	if total == 0 {
		return 0
	}
	return float64(bp.CacheHits+bp.SeqHits) / float64(total)
}
