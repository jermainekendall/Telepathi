package engine

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"

	"github.com/telepathi/pkg/gpuaccel"
	"github.com/telepathi/pkg/predictor"
)

// Block types sent on the wire
const (
	BlockTypeHash       byte = 'H' // Known block: send 8-byte hash only (4096:8 ratio)
	BlockTypeCompressed byte = 'C' // New block: send deflate-compressed data
	BlockTypeRaw        byte = 'R' // New block, incompressible: send raw
)

// BlockResult represents one encoded block
type BlockResult struct {
	Index   int
	Type    byte
	Hash    predictor.BlockHash
	Payload []byte // empty for hash-only, compressed or raw data otherwise
	RawSize int
}

// ParallelEncoder processes blocks using multiple goroutines and optional GPU
type ParallelEncoder struct {
	Pred    *predictor.BlockPredictor
	GPU     *gpuaccel.GPU
	Workers int

	// Stats (atomic)
	TotalBlocks    uint64
	HashHits       uint64
	CompressBlocks uint64
	RawBlocks      uint64
	BytesIn        uint64
	BytesOut       uint64
}

func NewParallelEncoder(pred *predictor.BlockPredictor, workers int, gpu *gpuaccel.GPU) *ParallelEncoder {
	if workers < 1 {
		workers = 1
	}
	return &ParallelEncoder{
		Pred:    pred,
		Workers: workers,
		GPU:     gpu,
	}
}

// processedBlock holds the result of parallel processing
type processedBlock struct {
	idx        int
	hash       predictor.BlockHash
	data       []byte // original block data (reference into chunk)
	compressed []byte // compressed version (nil if not worth it)
}

// EncodeChunk processes a data chunk into encoded block results.
// If GPU is available: GPU hashes + classifies, CPU compresses selectively.
// If CPU only: parallel hash + compress across workers.
func (pe *ParallelEncoder) EncodeChunk(data []byte) []BlockResult {
	blockSize := predictor.BlockSize
	nBlocks := (len(data) + blockSize - 1) / blockSize

	lastBlockSize := len(data) % blockSize
	if lastBlockSize == 0 {
		lastBlockSize = blockSize
	}

	// Pad data to full blocks for GPU (needs aligned input)
	paddedData := data
	if len(data)%blockSize != 0 {
		paddedData = make([]byte, nBlocks*blockSize)
		copy(paddedData, data)
	}

	// Try GPU path
	if pe.GPU != nil && pe.GPU.Available() && pe.GPU.IsHardwareGPU() {
		return pe.encodeChunkGPU(paddedData, data, nBlocks, lastBlockSize)
	}

	return pe.encodeChunkCPU(data, nBlocks)
}

// encodeChunkGPU uses the GPU for hashing and entropy classification
func (pe *ParallelEncoder) encodeChunkGPU(paddedData, origData []byte, nBlocks, lastBlockSize int) []BlockResult {
	blockSize := predictor.BlockSize
	results := make([]BlockResult, nBlocks)

	// GPU: hash all blocks + classify entropy in one submission
	hashes, compressible, err := pe.GPU.HashAndClassify(
		paddedData, nBlocks, lastBlockSize, 7.5,
	)

	if err != nil {
		// GPU failed, fall back to CPU
		return pe.encodeChunkCPU(origData, nBlocks)
	}

	// CPU: compress only the blocks the GPU flagged as compressible
	type compResult struct {
		idx        int
		compressed []byte
	}
	compChan := make(chan int, nBlocks)
	compResults := make([]compResult, 0)
	var compMu sync.Mutex
	var wg sync.WaitGroup

	// Queue compressible blocks for CPU compression
	for i := 0; i < nBlocks; i++ {
		if compressible[i] && !pe.Pred.Known(hashes[i]) {
			compChan <- i
		}
	}
	close(compChan)

	for w := 0; w < pe.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range compChan {
				start := idx * blockSize
				end := start + blockSize
				if end > len(origData) {
					end = len(origData)
				}
				block := origData[start:end]

				var buf bytes.Buffer
				w, _ := flate.NewWriter(&buf, flate.BestSpeed)
				w.Write(block)
				w.Close()

				if buf.Len() < len(block)*9/10 {
					compMu.Lock()
					compResults = append(compResults, compResult{idx, buf.Bytes()})
					compMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	// Build lookup of compression results
	compMap := make(map[int][]byte)
	for _, cr := range compResults {
		compMap[cr.idx] = cr.compressed
	}

	// Phase 2: Sequential classification
	for i := 0; i < nBlocks; i++ {
		start := i * blockSize
		end := start + blockSize
		if end > len(origData) {
			end = len(origData)
		}
		block := origData[start:end]
		hash := hashes[i]

		atomic.AddUint64(&pe.TotalBlocks, 1)
		atomic.AddUint64(&pe.BytesIn, uint64(len(block)))

		if pe.Pred.Known(hash) {
			results[i] = BlockResult{
				Index: i, Type: BlockTypeHash,
				Hash: hash, RawSize: len(block),
			}
			atomic.AddUint64(&pe.HashHits, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(predictor.HashSize))
			pe.Pred.Observe(hash, block)
		} else if comp, ok := compMap[i]; ok {
			results[i] = BlockResult{
				Index: i, Type: BlockTypeCompressed,
				Hash: hash, Payload: comp, RawSize: len(block),
			}
			atomic.AddUint64(&pe.CompressBlocks, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(len(comp)))
			pe.Pred.Observe(hash, block)
		} else {
			blockCopy := make([]byte, len(block))
			copy(blockCopy, block)
			results[i] = BlockResult{
				Index: i, Type: BlockTypeRaw,
				Hash: hash, Payload: blockCopy, RawSize: len(block),
			}
			atomic.AddUint64(&pe.RawBlocks, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(len(block)))
			pe.Pred.Observe(hash, block)
		}
	}

	return results
}

// encodeChunkCPU is the original CPU-only path
func (pe *ParallelEncoder) encodeChunkCPU(data []byte, nBlocks int) []BlockResult {
	blockSize := predictor.BlockSize
	processed := make([]processedBlock, nBlocks)
	results := make([]BlockResult, nBlocks)

	// Phase 1: Parallel hash + compress
	var wg sync.WaitGroup
	work := make(chan int, nBlocks)
	for i := 0; i < nBlocks; i++ {
		work <- i
	}
	close(work)

	for w := 0; w < pe.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				start := idx * blockSize
				end := start + blockSize
				if end > len(data) {
					end = len(data)
				}
				block := data[start:end]

				// Make a copy since data buffer may be reused
				blockCopy := make([]byte, len(block))
				copy(blockCopy, block)

				h := predictor.HashBlock(blockCopy)

				// Pre-compress the block using deflate (fast level)
				var comp []byte
				var buf bytes.Buffer
				w, _ := flate.NewWriter(&buf, flate.BestSpeed)
				w.Write(blockCopy)
				w.Close()
				if buf.Len() < len(blockCopy)*9/10 { // Only use if >=10% savings
					comp = buf.Bytes()
				}

				processed[idx] = processedBlock{
					idx:        idx,
					hash:       h,
					data:       blockCopy,
					compressed: comp,
				}
			}
		}()
	}
	wg.Wait()

	// Phase 2: Sequential classification using predictor
	for i := 0; i < nBlocks; i++ {
		pb := &processed[i]
		atomic.AddUint64(&pe.TotalBlocks, 1)
		atomic.AddUint64(&pe.BytesIn, uint64(len(pb.data)))

		if pe.Pred.Known(pb.hash) {
			// Cache hit: receiver already has this block
			results[i] = BlockResult{
				Index:   i,
				Type:    BlockTypeHash,
				Hash:    pb.hash,
				RawSize: len(pb.data),
			}
			atomic.AddUint64(&pe.HashHits, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(predictor.HashSize))
			// Still observe to keep sequence history in sync
			pe.Pred.Observe(pb.hash, pb.data)
		} else if pb.compressed != nil {
			// New block but compressible
			results[i] = BlockResult{
				Index:   i,
				Type:    BlockTypeCompressed,
				Hash:    pb.hash,
				Payload: pb.compressed,
				RawSize: len(pb.data),
			}
			atomic.AddUint64(&pe.CompressBlocks, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(len(pb.compressed)))
			pe.Pred.Observe(pb.hash, pb.data)
		} else {
			// New block, incompressible: send raw
			results[i] = BlockResult{
				Index:   i,
				Type:    BlockTypeRaw,
				Hash:    pb.hash,
				Payload: pb.data,
				RawSize: len(pb.data),
			}
			atomic.AddUint64(&pe.RawBlocks, 1)
			atomic.AddUint64(&pe.BytesOut, uint64(len(pb.data)))
			pe.Pred.Observe(pb.hash, pb.data)
		}
	}

	return results
}

// MarshalResults serializes encoded blocks for the wire
func MarshalResults(results []BlockResult) []byte {
	// Estimate size
	size := 4
	for _, r := range results {
		size += 1 + predictor.HashSize + 4 + len(r.Payload)
	}

	buf := make([]byte, 0, size)
	tmp := make([]byte, 4)

	binary.BigEndian.PutUint32(tmp, uint32(len(results)))
	buf = append(buf, tmp...)

	for _, r := range results {
		buf = append(buf, r.Type)
		buf = append(buf, r.Hash[:]...)
		binary.BigEndian.PutUint32(tmp, uint32(len(r.Payload)))
		buf = append(buf, tmp...)
		if len(r.Payload) > 0 {
			buf = append(buf, r.Payload...)
		}
	}

	return buf
}

// UnmarshalResults deserializes from wire format
func UnmarshalResults(data []byte) ([]BlockResult, error) {
	if len(data) < 4 {
		return nil, nil
	}

	nBlocks := int(binary.BigEndian.Uint32(data[:4]))
	results := make([]BlockResult, 0, nBlocks)
	pos := 4

	for i := 0; i < nBlocks && pos < len(data); i++ {
		r := BlockResult{Index: i}

		if pos >= len(data) {
			break
		}
		r.Type = data[pos]
		pos++

		if pos+predictor.HashSize > len(data) {
			break
		}
		copy(r.Hash[:], data[pos:pos+predictor.HashSize])
		pos += predictor.HashSize

		if pos+4 > len(data) {
			break
		}
		payloadLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4

		if payloadLen > 0 {
			if pos+payloadLen > len(data) {
				break
			}
			r.Payload = make([]byte, payloadLen)
			copy(r.Payload, data[pos:pos+payloadLen])
			pos += payloadLen
		}

		results = append(results, r)
	}

	return results, nil
}

// DecodeBlocks reconstructs original data from encoded blocks
func DecodeBlocks(results []BlockResult, pred *predictor.BlockPredictor, blockSize int) []byte {
	out := make([]byte, 0, len(results)*blockSize)

	for _, r := range results {
		switch r.Type {
		case BlockTypeHash:
			// Lookup from receiver's cache
			data, ok := pred.Lookup(r.Hash)
			if ok {
				out = append(out, data...)
				pred.Observe(r.Hash, data)
			} else {
				// Desync — pad with zeros (will cause checksum failure)
				out = append(out, make([]byte, blockSize)...)
			}

		case BlockTypeCompressed:
			// Decompress
			reader := flate.NewReader(bytes.NewReader(r.Payload))
			decompressed, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				// Decompression failed — use payload as-is
				out = append(out, r.Payload...)
			} else {
				out = append(out, decompressed...)
				pred.Observe(r.Hash, decompressed)
			}

		case BlockTypeRaw:
			out = append(out, r.Payload...)
			pred.Observe(r.Hash, r.Payload)
		}
	}

	return out
}
