package gpuaccel

/*
#cgo LDFLAGS: -lvulkan
#cgo CFLAGS: -I.
#include "gpu.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/telepathi/pkg/predictor"
)

// GPU wraps the Vulkan compute context
type GPU struct {
	ctx        *C.TgContext
	available  bool
	isHardware bool
	deviceName string
}

// Init attempts to initialize the Vulkan GPU accelerator.
// Returns a GPU handle. If initialization fails, the GPU
// handle is still valid but Available() returns false,
// and all operations fall back to CPU.
func Init() *GPU {
	g := &GPU{}

	var ctx *C.TgContext
	rc := C.tg_init(&ctx)
	if rc != C.TG_OK {
		g.available = false
		return g
	}

	g.ctx = ctx
	g.available = true
	g.deviceName = C.GoString(C.tg_device_name(ctx))
	g.isHardware = C.tg_is_hardware_gpu(ctx) != 0

	return g
}

// Destroy frees GPU resources
func (g *GPU) Destroy() {
	if g.ctx != nil {
		C.tg_destroy(g.ctx)
		g.ctx = nil
	}
}

// Available returns true if a GPU compute device was found
func (g *GPU) Available() bool {
	return g.available
}

// IsHardwareGPU returns true if the device is a real GPU (not llvmpipe)
func (g *GPU) IsHardwareGPU() bool {
	return g.isHardware
}

// DeviceName returns the Vulkan device name
func (g *GPU) DeviceName() string {
	return g.deviceName
}

// HashBlocks computes SHA-256 truncated hashes for multiple 4KB blocks on the GPU.
// data must be padded to a multiple of 4096 bytes (except the last block).
// Returns one BlockHash per block.
func (g *GPU) HashBlocks(data []byte, numBlocks int, lastBlockSize int) ([]predictor.BlockHash, error) {
	if !g.available || !g.isHardware {
		return nil, fmt.Errorf("GPU not available")
	}

	if lastBlockSize == 0 {
		lastBlockSize = predictor.BlockSize
	}

	outBuf := make([]byte, numBlocks*predictor.HashSize)

	rc := C.tg_hash_blocks(
		g.ctx,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint32_t(numBlocks),
		C.uint32_t(lastBlockSize),
		(*C.uint8_t)(unsafe.Pointer(&outBuf[0])),
	)

	if rc != C.TG_OK {
		return nil, fmt.Errorf("GPU hash failed: error %d", rc)
	}

	// Convert output bytes to BlockHash array
	hashes := make([]predictor.BlockHash, numBlocks)
	for i := 0; i < numBlocks; i++ {
		copy(hashes[i][:], outBuf[i*predictor.HashSize:(i+1)*predictor.HashSize])
	}

	return hashes, nil
}

// ClassifyEntropy classifies blocks as compressible or incompressible on the GPU.
// Returns a bool slice: true = compressible, false = incompressible.
func (g *GPU) ClassifyEntropy(data []byte, numBlocks int, lastBlockSize int, threshold float32) ([]bool, error) {
	if !g.available || !g.isHardware {
		return nil, fmt.Errorf("GPU not available")
	}

	if lastBlockSize == 0 {
		lastBlockSize = predictor.BlockSize
	}

	flags := make([]uint32, numBlocks)

	rc := C.tg_classify_entropy(
		g.ctx,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint32_t(numBlocks),
		C.uint32_t(lastBlockSize),
		C.float(threshold),
		(*C.uint32_t)(unsafe.Pointer(&flags[0])),
	)

	if rc != C.TG_OK {
		return nil, fmt.Errorf("GPU entropy classification failed: error %d", rc)
	}

	result := make([]bool, numBlocks)
	for i, f := range flags {
		result[i] = f != 0
	}
	return result, nil
}

// HashAndClassify combines both operations with a single data upload
func (g *GPU) HashAndClassify(data []byte, numBlocks int, lastBlockSize int, threshold float32) ([]predictor.BlockHash, []bool, error) {
	if !g.available || !g.isHardware {
		return nil, nil, fmt.Errorf("GPU not available")
	}

	if lastBlockSize == 0 {
		lastBlockSize = predictor.BlockSize
	}

	outHashes := make([]byte, numBlocks*predictor.HashSize)
	outFlags := make([]uint32, numBlocks)

	rc := C.tg_hash_and_classify(
		g.ctx,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint32_t(numBlocks),
		C.uint32_t(lastBlockSize),
		C.float(threshold),
		(*C.uint8_t)(unsafe.Pointer(&outHashes[0])),
		(*C.uint32_t)(unsafe.Pointer(&outFlags[0])),
	)

	if rc != C.TG_OK {
		return nil, nil, fmt.Errorf("GPU hash+classify failed: error %d", rc)
	}

	hashes := make([]predictor.BlockHash, numBlocks)
	for i := 0; i < numBlocks; i++ {
		copy(hashes[i][:], outHashes[i*predictor.HashSize:(i+1)*predictor.HashSize])
	}

	compressible := make([]bool, numBlocks)
	for i, f := range outFlags {
		compressible[i] = f != 0
	}

	return hashes, compressible, nil
}
