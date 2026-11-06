#ifndef TELEPATHI_GPU_H
#define TELEPATHI_GPU_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Error codes
#define TG_OK                0
#define TG_ERR_NO_VULKAN    -1
#define TG_ERR_NO_DEVICE    -2
#define TG_ERR_INIT_FAILED  -3
#define TG_ERR_SHADER       -4
#define TG_ERR_MEMORY       -5
#define TG_ERR_COMPUTE      -6

// Opaque handle to GPU context
typedef struct TgContext TgContext;

// Initialize Vulkan compute context
// Returns TG_OK on success, error code on failure
int tg_init(TgContext** ctx);

// Destroy context and free GPU resources
void tg_destroy(TgContext* ctx);

// Hash multiple 4KB blocks in parallel on the GPU
// blocks: input data (numBlocks * 4096 bytes, last block may be smaller)
// numBlocks: number of blocks
// lastBlockSize: size of the last block in bytes (0 means full 4096)
// outHashes: output buffer for 8-byte hashes (numBlocks * 8 bytes, caller-allocated)
// Returns TG_OK on success
int tg_hash_blocks(TgContext* ctx,
                   const uint8_t* blocks,
                   uint32_t numBlocks,
                   uint32_t lastBlockSize,
                   uint8_t* outHashes);

// Classify blocks by entropy (compressible vs incompressible)
// blocks: input data
// numBlocks: number of blocks
// lastBlockSize: size of last block
// entropyThreshold: blocks below this are compressible (7.5 is a good default)
// outFlags: output buffer (numBlocks * 4 bytes uint32), 1=compressible, 0=not
// Returns TG_OK on success
int tg_classify_entropy(TgContext* ctx,
                        const uint8_t* blocks,
                        uint32_t numBlocks,
                        uint32_t lastBlockSize,
                        float entropyThreshold,
                        uint32_t* outFlags);

// Combined: hash + classify in one GPU submission (fewer transfers)
int tg_hash_and_classify(TgContext* ctx,
                         const uint8_t* blocks,
                         uint32_t numBlocks,
                         uint32_t lastBlockSize,
                         float entropyThreshold,
                         uint8_t* outHashes,
                         uint32_t* outFlags);

// Query GPU info
const char* tg_device_name(TgContext* ctx);
int tg_is_hardware_gpu(TgContext* ctx);

#ifdef __cplusplus
}
#endif

#endif // TELEPATHI_GPU_H
