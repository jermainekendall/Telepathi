#include "gpu.h"

#include <vulkan/vulkan.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Embedded SPIR-V will be loaded from files at runtime
// In production, these would be compiled at build time with glslangValidator

struct TgContext {
    VkInstance instance;
    VkPhysicalDevice physicalDevice;
    VkDevice device;
    VkQueue computeQueue;
    uint32_t computeQueueFamily;

    // Pipelines
    VkPipelineLayout hashPipelineLayout;
    VkPipeline hashPipeline;
    VkDescriptorSetLayout hashDescSetLayout;
    VkDescriptorPool hashDescPool;

    VkPipelineLayout entropyPipelineLayout;
    VkPipeline entropyPipeline;
    VkDescriptorSetLayout entropyDescSetLayout;
    VkDescriptorPool entropyDescPool;

    VkCommandPool commandPool;

    // Reusable buffers (resized as needed)
    VkBuffer inputBuffer;
    VkDeviceMemory inputMemory;
    VkDeviceSize inputBufferSize;

    VkBuffer hashOutputBuffer;
    VkDeviceMemory hashOutputMemory;
    VkDeviceSize hashOutputSize;

    VkBuffer entropyOutputBuffer;
    VkDeviceMemory entropyOutputMemory;
    VkDeviceSize entropyOutputSize;

    // Device info
    char deviceName[256];
    VkPhysicalDeviceType deviceType;
};

// Forward declarations
static int findComputeQueue(VkPhysicalDevice device, uint32_t* queueFamily);
static uint32_t findMemoryType(VkPhysicalDevice physDev, uint32_t typeFilter, VkMemoryPropertyFlags props);
static int createBuffer(TgContext* ctx, VkDeviceSize size, VkBufferUsageFlags usage,
                        VkMemoryPropertyFlags memProps, VkBuffer* buffer, VkDeviceMemory* memory);
static int ensureInputBuffer(TgContext* ctx, VkDeviceSize size);
static int ensureHashOutputBuffer(TgContext* ctx, VkDeviceSize size);
static int ensureEntropyOutputBuffer(TgContext* ctx, VkDeviceSize size);
static VkShaderModule loadShaderFromFile(VkDevice device, const char* path);
static int createHashPipeline(TgContext* ctx, const char* shaderPath);
static int createEntropyPipeline(TgContext* ctx, const char* shaderPath);

int tg_init(TgContext** outCtx) {
    TgContext* ctx = (TgContext*)calloc(1, sizeof(TgContext));
    if (!ctx) return TG_ERR_MEMORY;

    // Create Vulkan instance
    VkApplicationInfo appInfo = {
        .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
        .pApplicationName = "Telepathi GPU Accel",
        .applicationVersion = VK_MAKE_VERSION(1, 0, 0),
        .pEngineName = "Telepathi",
        .engineVersion = VK_MAKE_VERSION(1, 0, 0),
        .apiVersion = VK_API_VERSION_1_2,
    };

    VkInstanceCreateInfo createInfo = {
        .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
        .pApplicationInfo = &appInfo,
    };

    if (vkCreateInstance(&createInfo, NULL, &ctx->instance) != VK_SUCCESS) {
        free(ctx);
        return TG_ERR_NO_VULKAN;
    }

    // Find a physical device with compute support
    uint32_t deviceCount = 0;
    vkEnumeratePhysicalDevices(ctx->instance, &deviceCount, NULL);
    if (deviceCount == 0) {
        vkDestroyInstance(ctx->instance, NULL);
        free(ctx);
        return TG_ERR_NO_DEVICE;
    }

    VkPhysicalDevice* devices = (VkPhysicalDevice*)malloc(sizeof(VkPhysicalDevice) * deviceCount);
    vkEnumeratePhysicalDevices(ctx->instance, &deviceCount, devices);

    // Prefer discrete GPU, then integrated, then any with compute
    int bestIdx = -1;
    int bestScore = -1;
    for (uint32_t i = 0; i < deviceCount; i++) {
        uint32_t qf;
        if (findComputeQueue(devices[i], &qf) != 0) continue;

        VkPhysicalDeviceProperties props;
        vkGetPhysicalDeviceProperties(devices[i], &props);

        int score = 0;
        if (props.deviceType == VK_PHYSICAL_DEVICE_TYPE_DISCRETE_GPU) score = 3;
        else if (props.deviceType == VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU) score = 2;
        else if (props.deviceType == VK_PHYSICAL_DEVICE_TYPE_VIRTUAL_GPU) score = 1;
        else score = 0; // CPU fallback (llvmpipe)

        if (score > bestScore) {
            bestScore = score;
            bestIdx = (int)i;
        }
    }

    if (bestIdx < 0) {
        free(devices);
        vkDestroyInstance(ctx->instance, NULL);
        free(ctx);
        return TG_ERR_NO_DEVICE;
    }

    ctx->physicalDevice = devices[bestIdx];
    free(devices);

    // Get device properties
    VkPhysicalDeviceProperties devProps;
    vkGetPhysicalDeviceProperties(ctx->physicalDevice, &devProps);
    strncpy(ctx->deviceName, devProps.deviceName, sizeof(ctx->deviceName) - 1);
    ctx->deviceType = devProps.deviceType;

    // Find compute queue
    findComputeQueue(ctx->physicalDevice, &ctx->computeQueueFamily);

    // Create logical device
    float priority = 1.0f;
    VkDeviceQueueCreateInfo queueInfo = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
        .queueFamilyIndex = ctx->computeQueueFamily,
        .queueCount = 1,
        .pQueuePriorities = &priority,
    };

    VkDeviceCreateInfo devCreateInfo = {
        .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
        .queueCreateInfoCount = 1,
        .pQueueCreateInfos = &queueInfo,
    };

    if (vkCreateDevice(ctx->physicalDevice, &devCreateInfo, NULL, &ctx->device) != VK_SUCCESS) {
        vkDestroyInstance(ctx->instance, NULL);
        free(ctx);
        return TG_ERR_INIT_FAILED;
    }

    vkGetDeviceQueue(ctx->device, ctx->computeQueueFamily, 0, &ctx->computeQueue);

    // Create command pool
    VkCommandPoolCreateInfo poolInfo = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
        .queueFamilyIndex = ctx->computeQueueFamily,
        .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
    };

    if (vkCreateCommandPool(ctx->device, &poolInfo, NULL, &ctx->commandPool) != VK_SUCCESS) {
        vkDestroyDevice(ctx->device, NULL);
        vkDestroyInstance(ctx->instance, NULL);
        free(ctx);
        return TG_ERR_INIT_FAILED;
    }

    // Create pipelines — try multiple shader locations
    const char* shaderPaths[] = {
        "shaders/sha256_blocks.spv",
        "/usr/local/share/telepathi/shaders/sha256_blocks.spv",
        NULL
    };
    const char* entropyPaths[] = {
        "shaders/entropy_classify.spv",
        "/usr/local/share/telepathi/shaders/entropy_classify.spv",
        NULL
    };

    int hashOk = TG_ERR_SHADER;
    for (int i = 0; shaderPaths[i] != NULL; i++) {
        if ((hashOk = createHashPipeline(ctx, shaderPaths[i])) == TG_OK) break;
    }
    if (hashOk != TG_OK) {
        fprintf(stderr, "Note: SHA-256 GPU shader not found, will use CPU\n");
    }

    int entropyOk = TG_ERR_SHADER;
    for (int i = 0; entropyPaths[i] != NULL; i++) {
        if ((entropyOk = createEntropyPipeline(ctx, entropyPaths[i])) == TG_OK) break;
    }
    if (entropyOk != TG_OK) {
        fprintf(stderr, "Note: Entropy GPU shader not found, will use CPU\n");
    }

    *outCtx = ctx;
    return TG_OK;
}

void tg_destroy(TgContext* ctx) {
    if (!ctx) return;

    vkDeviceWaitIdle(ctx->device);

    // Free buffers
    if (ctx->inputBuffer) vkDestroyBuffer(ctx->device, ctx->inputBuffer, NULL);
    if (ctx->inputMemory) vkFreeMemory(ctx->device, ctx->inputMemory, NULL);
    if (ctx->hashOutputBuffer) vkDestroyBuffer(ctx->device, ctx->hashOutputBuffer, NULL);
    if (ctx->hashOutputMemory) vkFreeMemory(ctx->device, ctx->hashOutputMemory, NULL);
    if (ctx->entropyOutputBuffer) vkDestroyBuffer(ctx->device, ctx->entropyOutputBuffer, NULL);
    if (ctx->entropyOutputMemory) vkFreeMemory(ctx->device, ctx->entropyOutputMemory, NULL);

    // Free pipelines
    if (ctx->hashPipeline) vkDestroyPipeline(ctx->device, ctx->hashPipeline, NULL);
    if (ctx->hashPipelineLayout) vkDestroyPipelineLayout(ctx->device, ctx->hashPipelineLayout, NULL);
    if (ctx->hashDescSetLayout) vkDestroyDescriptorSetLayout(ctx->device, ctx->hashDescSetLayout, NULL);
    if (ctx->hashDescPool) vkDestroyDescriptorPool(ctx->device, ctx->hashDescPool, NULL);

    if (ctx->entropyPipeline) vkDestroyPipeline(ctx->device, ctx->entropyPipeline, NULL);
    if (ctx->entropyPipelineLayout) vkDestroyPipelineLayout(ctx->device, ctx->entropyPipelineLayout, NULL);
    if (ctx->entropyDescSetLayout) vkDestroyDescriptorSetLayout(ctx->device, ctx->entropyDescSetLayout, NULL);
    if (ctx->entropyDescPool) vkDestroyDescriptorPool(ctx->device, ctx->entropyDescPool, NULL);

    if (ctx->commandPool) vkDestroyCommandPool(ctx->device, ctx->commandPool, NULL);
    if (ctx->device) vkDestroyDevice(ctx->device, NULL);
    if (ctx->instance) vkDestroyInstance(ctx->instance, NULL);

    free(ctx);
}

int tg_hash_blocks(TgContext* ctx, const uint8_t* blocks, uint32_t numBlocks,
                   uint32_t lastBlockSize, uint8_t* outHashes) {
    if (!ctx || !ctx->hashPipeline) return TG_ERR_COMPUTE;

    VkDeviceSize inputSize = (VkDeviceSize)numBlocks * 4096;
    VkDeviceSize outputSize = (VkDeviceSize)numBlocks * 8;

    // Ensure buffers are large enough
    if (ensureInputBuffer(ctx, inputSize) != 0) return TG_ERR_MEMORY;
    if (ensureHashOutputBuffer(ctx, outputSize) != 0) return TG_ERR_MEMORY;

    // Upload input data
    void* mapped;
    vkMapMemory(ctx->device, ctx->inputMemory, 0, inputSize, 0, &mapped);
    memcpy(mapped, blocks, inputSize);
    vkUnmapMemory(ctx->device, ctx->inputMemory);

    // Update descriptor set
    VkDescriptorSet descSet;
    VkDescriptorSetAllocateInfo allocInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
        .descriptorPool = ctx->hashDescPool,
        .descriptorSetCount = 1,
        .pSetLayouts = &ctx->hashDescSetLayout,
    };
    vkAllocateDescriptorSets(ctx->device, &allocInfo, &descSet);

    VkDescriptorBufferInfo inputBufferInfo = { ctx->inputBuffer, 0, inputSize };
    VkDescriptorBufferInfo outputBufferInfo = { ctx->hashOutputBuffer, 0, outputSize };

    VkWriteDescriptorSet writes[2] = {
        {
            .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
            .dstSet = descSet,
            .dstBinding = 0,
            .descriptorCount = 1,
            .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
            .pBufferInfo = &inputBufferInfo,
        },
        {
            .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
            .dstSet = descSet,
            .dstBinding = 1,
            .descriptorCount = 1,
            .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
            .pBufferInfo = &outputBufferInfo,
        },
    };
    vkUpdateDescriptorSets(ctx->device, 2, writes, 0, NULL);

    // Record and submit command buffer
    VkCommandBuffer cmdBuf;
    VkCommandBufferAllocateInfo cmdAllocInfo = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
        .commandPool = ctx->commandPool,
        .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
        .commandBufferCount = 1,
    };
    vkAllocateCommandBuffers(ctx->device, &cmdAllocInfo, &cmdBuf);

    VkCommandBufferBeginInfo beginInfo = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
        .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
    };
    vkBeginCommandBuffer(cmdBuf, &beginInfo);

    vkCmdBindPipeline(cmdBuf, VK_PIPELINE_BIND_POINT_COMPUTE, ctx->hashPipeline);
    vkCmdBindDescriptorSets(cmdBuf, VK_PIPELINE_BIND_POINT_COMPUTE,
                            ctx->hashPipelineLayout, 0, 1, &descSet, 0, NULL);

    // Push constants: numBlocks, lastBlockSize
    uint32_t pushData[2] = { numBlocks, lastBlockSize ? lastBlockSize : 4096 };
    vkCmdPushConstants(cmdBuf, ctx->hashPipelineLayout,
                       VK_SHADER_STAGE_COMPUTE_BIT, 0, 8, pushData);

    // Dispatch one workgroup per block
    vkCmdDispatch(cmdBuf, numBlocks, 1, 1);

    // Memory barrier to ensure compute writes are visible for host read
    VkMemoryBarrier barrier = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT,
        .dstAccessMask = VK_ACCESS_HOST_READ_BIT,
    };
    vkCmdPipelineBarrier(cmdBuf,
                         VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,
                         VK_PIPELINE_STAGE_HOST_BIT,
                         0, 1, &barrier, 0, NULL, 0, NULL);

    vkEndCommandBuffer(cmdBuf);

    // Submit and wait
    VkSubmitInfo submitInfo = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1,
        .pCommandBuffers = &cmdBuf,
    };

    VkFence fence;
    VkFenceCreateInfo fenceInfo = { .sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO };
    vkCreateFence(ctx->device, &fenceInfo, NULL, &fence);

    vkQueueSubmit(ctx->computeQueue, 1, &submitInfo, fence);
    vkWaitForFences(ctx->device, 1, &fence, VK_TRUE, UINT64_MAX);

    // Read back results
    vkMapMemory(ctx->device, ctx->hashOutputMemory, 0, outputSize, 0, &mapped);
    memcpy(outHashes, mapped, outputSize);
    vkUnmapMemory(ctx->device, ctx->hashOutputMemory);

    // Cleanup
    vkDestroyFence(ctx->device, fence, NULL);
    vkFreeCommandBuffers(ctx->device, ctx->commandPool, 1, &cmdBuf);
    vkResetDescriptorPool(ctx->device, ctx->hashDescPool, 0);

    return TG_OK;
}

int tg_classify_entropy(TgContext* ctx, const uint8_t* blocks, uint32_t numBlocks,
                        uint32_t lastBlockSize, float entropyThreshold, uint32_t* outFlags) {
    if (!ctx || !ctx->entropyPipeline) return TG_ERR_COMPUTE;

    VkDeviceSize inputSize = (VkDeviceSize)numBlocks * 4096;
    VkDeviceSize outputSize = (VkDeviceSize)numBlocks * sizeof(uint32_t);

    if (ensureInputBuffer(ctx, inputSize) != 0) return TG_ERR_MEMORY;
    if (ensureEntropyOutputBuffer(ctx, outputSize) != 0) return TG_ERR_MEMORY;

    // Upload input (may already be uploaded if called after tg_hash_blocks)
    void* mapped;
    vkMapMemory(ctx->device, ctx->inputMemory, 0, inputSize, 0, &mapped);
    memcpy(mapped, blocks, inputSize);
    vkUnmapMemory(ctx->device, ctx->inputMemory);

    // Descriptor set
    VkDescriptorSet descSet;
    VkDescriptorSetAllocateInfo allocInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
        .descriptorPool = ctx->entropyDescPool,
        .descriptorSetCount = 1,
        .pSetLayouts = &ctx->entropyDescSetLayout,
    };
    vkAllocateDescriptorSets(ctx->device, &allocInfo, &descSet);

    VkDescriptorBufferInfo inputBufferInfo = { ctx->inputBuffer, 0, inputSize };
    VkDescriptorBufferInfo outputBufferInfo = { ctx->entropyOutputBuffer, 0, outputSize };

    VkWriteDescriptorSet writes[2] = {
        {
            .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
            .dstSet = descSet, .dstBinding = 0, .descriptorCount = 1,
            .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
            .pBufferInfo = &inputBufferInfo,
        },
        {
            .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
            .dstSet = descSet, .dstBinding = 1, .descriptorCount = 1,
            .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
            .pBufferInfo = &outputBufferInfo,
        },
    };
    vkUpdateDescriptorSets(ctx->device, 2, writes, 0, NULL);

    // Command buffer
    VkCommandBuffer cmdBuf;
    VkCommandBufferAllocateInfo cmdAllocInfo = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
        .commandPool = ctx->commandPool,
        .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
        .commandBufferCount = 1,
    };
    vkAllocateCommandBuffers(ctx->device, &cmdAllocInfo, &cmdBuf);

    VkCommandBufferBeginInfo beginInfo = {
        .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
        .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
    };
    vkBeginCommandBuffer(cmdBuf, &beginInfo);

    vkCmdBindPipeline(cmdBuf, VK_PIPELINE_BIND_POINT_COMPUTE, ctx->entropyPipeline);
    vkCmdBindDescriptorSets(cmdBuf, VK_PIPELINE_BIND_POINT_COMPUTE,
                            ctx->entropyPipelineLayout, 0, 1, &descSet, 0, NULL);

    // Push constants: numBlocks, lastBlockSize, entropyThreshold
    struct { uint32_t numBlocks; uint32_t lastBlockSize; float threshold; } pushData = {
        numBlocks, lastBlockSize ? lastBlockSize : 4096, entropyThreshold
    };
    vkCmdPushConstants(cmdBuf, ctx->entropyPipelineLayout,
                       VK_SHADER_STAGE_COMPUTE_BIT, 0, sizeof(pushData), &pushData);

    vkCmdDispatch(cmdBuf, numBlocks, 1, 1);

    VkMemoryBarrier barrier = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT,
        .dstAccessMask = VK_ACCESS_HOST_READ_BIT,
    };
    vkCmdPipelineBarrier(cmdBuf, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,
                         VK_PIPELINE_STAGE_HOST_BIT, 0, 1, &barrier, 0, NULL, 0, NULL);

    vkEndCommandBuffer(cmdBuf);

    VkFence fence;
    VkFenceCreateInfo fenceInfo = { .sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO };
    vkCreateFence(ctx->device, &fenceInfo, NULL, &fence);

    VkSubmitInfo submitInfo = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
        .commandBufferCount = 1, .pCommandBuffers = &cmdBuf,
    };
    vkQueueSubmit(ctx->computeQueue, 1, &submitInfo, fence);
    vkWaitForFences(ctx->device, 1, &fence, VK_TRUE, UINT64_MAX);

    vkMapMemory(ctx->device, ctx->entropyOutputMemory, 0, outputSize, 0, &mapped);
    memcpy(outFlags, mapped, outputSize);
    vkUnmapMemory(ctx->device, ctx->entropyOutputMemory);

    vkDestroyFence(ctx->device, fence, NULL);
    vkFreeCommandBuffers(ctx->device, ctx->commandPool, 1, &cmdBuf);
    vkResetDescriptorPool(ctx->device, ctx->entropyDescPool, 0);

    return TG_OK;
}

int tg_hash_and_classify(TgContext* ctx, const uint8_t* blocks, uint32_t numBlocks,
                         uint32_t lastBlockSize, float entropyThreshold,
                         uint8_t* outHashes, uint32_t* outFlags) {
    // Upload data once, run both pipelines
    int rc = tg_hash_blocks(ctx, blocks, numBlocks, lastBlockSize, outHashes);
    if (rc != TG_OK) return rc;

    // Input is already uploaded, but tg_classify_entropy re-uploads (simple but suboptimal)
    // A production version would share the input buffer between both dispatches
    return tg_classify_entropy(ctx, blocks, numBlocks, lastBlockSize, entropyThreshold, outFlags);
}

const char* tg_device_name(TgContext* ctx) {
    return ctx ? ctx->deviceName : "none";
}

int tg_is_hardware_gpu(TgContext* ctx) {
    if (!ctx) return 0;
    return ctx->deviceType == VK_PHYSICAL_DEVICE_TYPE_DISCRETE_GPU ||
           ctx->deviceType == VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU;
}

// ============ Internal helpers ============

static int findComputeQueue(VkPhysicalDevice device, uint32_t* queueFamily) {
    uint32_t count = 0;
    vkGetPhysicalDeviceQueueFamilyProperties(device, &count, NULL);
    VkQueueFamilyProperties* props = (VkQueueFamilyProperties*)malloc(sizeof(VkQueueFamilyProperties) * count);
    vkGetPhysicalDeviceQueueFamilyProperties(device, &count, props);

    for (uint32_t i = 0; i < count; i++) {
        if (props[i].queueFlags & VK_QUEUE_COMPUTE_BIT) {
            *queueFamily = i;
            free(props);
            return 0;
        }
    }
    free(props);
    return -1;
}

static uint32_t findMemoryType(VkPhysicalDevice physDev, uint32_t typeFilter, VkMemoryPropertyFlags props) {
    VkPhysicalDeviceMemoryProperties memProps;
    vkGetPhysicalDeviceMemoryProperties(physDev, &memProps);

    for (uint32_t i = 0; i < memProps.memoryTypeCount; i++) {
        if ((typeFilter & (1 << i)) &&
            (memProps.memoryTypes[i].propertyFlags & props) == props) {
            return i;
        }
    }
    return UINT32_MAX;
}

static int createBuffer(TgContext* ctx, VkDeviceSize size, VkBufferUsageFlags usage,
                        VkMemoryPropertyFlags memProps, VkBuffer* buffer, VkDeviceMemory* memory) {
    VkBufferCreateInfo bufInfo = {
        .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
        .size = size,
        .usage = usage,
        .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
    };

    if (vkCreateBuffer(ctx->device, &bufInfo, NULL, buffer) != VK_SUCCESS)
        return TG_ERR_MEMORY;

    VkMemoryRequirements memReqs;
    vkGetBufferMemoryRequirements(ctx->device, *buffer, &memReqs);

    VkMemoryAllocateInfo allocInfo = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
        .allocationSize = memReqs.size,
        .memoryTypeIndex = findMemoryType(ctx->physicalDevice, memReqs.memoryTypeBits, memProps),
    };

    if (allocInfo.memoryTypeIndex == UINT32_MAX) return TG_ERR_MEMORY;
    if (vkAllocateMemory(ctx->device, &allocInfo, NULL, memory) != VK_SUCCESS) return TG_ERR_MEMORY;
    vkBindBufferMemory(ctx->device, *buffer, *memory, 0);

    return TG_OK;
}

static int ensureInputBuffer(TgContext* ctx, VkDeviceSize size) {
    if (ctx->inputBufferSize >= size) return TG_OK;
    if (ctx->inputBuffer) { vkDestroyBuffer(ctx->device, ctx->inputBuffer, NULL); ctx->inputBuffer = VK_NULL_HANDLE; }
    if (ctx->inputMemory) { vkFreeMemory(ctx->device, ctx->inputMemory, NULL); ctx->inputMemory = VK_NULL_HANDLE; }

    int rc = createBuffer(ctx, size, VK_BUFFER_USAGE_STORAGE_BUFFER_BIT,
                          VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT,
                          &ctx->inputBuffer, &ctx->inputMemory);
    if (rc == TG_OK) ctx->inputBufferSize = size;
    return rc;
}

static int ensureHashOutputBuffer(TgContext* ctx, VkDeviceSize size) {
    if (ctx->hashOutputSize >= size) return TG_OK;
    if (ctx->hashOutputBuffer) { vkDestroyBuffer(ctx->device, ctx->hashOutputBuffer, NULL); }
    if (ctx->hashOutputMemory) { vkFreeMemory(ctx->device, ctx->hashOutputMemory, NULL); }

    int rc = createBuffer(ctx, size, VK_BUFFER_USAGE_STORAGE_BUFFER_BIT,
                          VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT,
                          &ctx->hashOutputBuffer, &ctx->hashOutputMemory);
    if (rc == TG_OK) ctx->hashOutputSize = size;
    return rc;
}

static int ensureEntropyOutputBuffer(TgContext* ctx, VkDeviceSize size) {
    if (ctx->entropyOutputSize >= size) return TG_OK;
    if (ctx->entropyOutputBuffer) { vkDestroyBuffer(ctx->device, ctx->entropyOutputBuffer, NULL); }
    if (ctx->entropyOutputMemory) { vkFreeMemory(ctx->device, ctx->entropyOutputMemory, NULL); }

    int rc = createBuffer(ctx, size, VK_BUFFER_USAGE_STORAGE_BUFFER_BIT,
                          VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT,
                          &ctx->entropyOutputBuffer, &ctx->entropyOutputMemory);
    if (rc == TG_OK) ctx->entropyOutputSize = size;
    return rc;
}

static VkShaderModule loadShaderFromFile(VkDevice device, const char* path) {
    FILE* f = fopen(path, "rb");
    if (!f) return VK_NULL_HANDLE;

    fseek(f, 0, SEEK_END);
    long size = ftell(f);
    fseek(f, 0, SEEK_SET);

    uint32_t* code = (uint32_t*)malloc(size);
    fread(code, 1, size, f);
    fclose(f);

    VkShaderModuleCreateInfo createInfo = {
        .sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO,
        .codeSize = (size_t)size,
        .pCode = code,
    };

    VkShaderModule module;
    VkResult result = vkCreateShaderModule(device, &createInfo, NULL, &module);
    free(code);

    return (result == VK_SUCCESS) ? module : VK_NULL_HANDLE;
}

static int createHashPipeline(TgContext* ctx, const char* shaderPath) {
    VkShaderModule shader = loadShaderFromFile(ctx->device, shaderPath);
    if (shader == VK_NULL_HANDLE) return TG_ERR_SHADER;

    // Descriptor set layout: 2 storage buffers
    VkDescriptorSetLayoutBinding bindings[2] = {
        { .binding = 0, .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
          .descriptorCount = 1, .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT },
        { .binding = 1, .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
          .descriptorCount = 1, .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT },
    };

    VkDescriptorSetLayoutCreateInfo layoutInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
        .bindingCount = 2, .pBindings = bindings,
    };
    vkCreateDescriptorSetLayout(ctx->device, &layoutInfo, NULL, &ctx->hashDescSetLayout);

    // Push constant range
    VkPushConstantRange pushRange = {
        .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
        .offset = 0, .size = 8, // 2 x uint32
    };

    VkPipelineLayoutCreateInfo pipeLayoutInfo = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
        .setLayoutCount = 1, .pSetLayouts = &ctx->hashDescSetLayout,
        .pushConstantRangeCount = 1, .pPushConstantRanges = &pushRange,
    };
    vkCreatePipelineLayout(ctx->device, &pipeLayoutInfo, NULL, &ctx->hashPipelineLayout);

    // Compute pipeline
    VkComputePipelineCreateInfo pipeInfo = {
        .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
        .stage = {
            .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
            .stage = VK_SHADER_STAGE_COMPUTE_BIT,
            .module = shader,
            .pName = "main",
        },
        .layout = ctx->hashPipelineLayout,
    };
    VkResult rc = vkCreateComputePipelines(ctx->device, VK_NULL_HANDLE, 1, &pipeInfo, NULL, &ctx->hashPipeline);

    vkDestroyShaderModule(ctx->device, shader, NULL);

    // Descriptor pool
    VkDescriptorPoolSize poolSize = {
        .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, .descriptorCount = 4,
    };
    VkDescriptorPoolCreateInfo dpInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
        .flags = VK_DESCRIPTOR_POOL_CREATE_FREE_DESCRIPTOR_SET_BIT,
        .maxSets = 2, .poolSizeCount = 1, .pPoolSizes = &poolSize,
    };
    vkCreateDescriptorPool(ctx->device, &dpInfo, NULL, &ctx->hashDescPool);

    return (rc == VK_SUCCESS) ? TG_OK : TG_ERR_SHADER;
}

static int createEntropyPipeline(TgContext* ctx, const char* shaderPath) {
    VkShaderModule shader = loadShaderFromFile(ctx->device, shaderPath);
    if (shader == VK_NULL_HANDLE) return TG_ERR_SHADER;

    VkDescriptorSetLayoutBinding bindings[2] = {
        { .binding = 0, .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
          .descriptorCount = 1, .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT },
        { .binding = 1, .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
          .descriptorCount = 1, .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT },
    };

    VkDescriptorSetLayoutCreateInfo layoutInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
        .bindingCount = 2, .pBindings = bindings,
    };
    vkCreateDescriptorSetLayout(ctx->device, &layoutInfo, NULL, &ctx->entropyDescSetLayout);

    VkPushConstantRange pushRange = {
        .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
        .offset = 0, .size = 12, // 2 x uint32 + 1 float
    };

    VkPipelineLayoutCreateInfo pipeLayoutInfo = {
        .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
        .setLayoutCount = 1, .pSetLayouts = &ctx->entropyDescSetLayout,
        .pushConstantRangeCount = 1, .pPushConstantRanges = &pushRange,
    };
    vkCreatePipelineLayout(ctx->device, &pipeLayoutInfo, NULL, &ctx->entropyPipelineLayout);

    VkComputePipelineCreateInfo pipeInfo = {
        .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
        .stage = {
            .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
            .stage = VK_SHADER_STAGE_COMPUTE_BIT,
            .module = shader,
            .pName = "main",
        },
        .layout = ctx->entropyPipelineLayout,
    };
    VkResult rc = vkCreateComputePipelines(ctx->device, VK_NULL_HANDLE, 1, &pipeInfo, NULL, &ctx->entropyPipeline);

    vkDestroyShaderModule(ctx->device, shader, NULL);

    VkDescriptorPoolSize poolSize = {
        .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, .descriptorCount = 4,
    };
    VkDescriptorPoolCreateInfo dpInfo = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
        .flags = VK_DESCRIPTOR_POOL_CREATE_FREE_DESCRIPTOR_SET_BIT,
        .maxSets = 2, .poolSizeCount = 1, .pPoolSizes = &poolSize,
    };
    vkCreateDescriptorPool(ctx->device, &dpInfo, NULL, &ctx->entropyDescPool);

    return (rc == VK_SUCCESS) ? TG_OK : TG_ERR_SHADER;
}
