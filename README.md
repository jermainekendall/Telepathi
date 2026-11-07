# Telepathi Protocol

**Predictive Block-Level File Transfer for Distributed Storage**

The Telepathi Protocol minimizes bandwidth by recognizing and deduplicating
data blocks that have been seen before. Instead of sending raw data, it sends
short hash references for known blocks and only transmits full data for
genuinely new blocks.

## How It Works

1. **Shared Seed**: Both endpoints exchange a cryptographic seed during handshake
2. **Pre-Training**: Both sides seed their block cache with common patterns (zero blocks, etc.)
3. **Block Hashing**: File data is split into 4KB blocks, hashed in parallel across CPU cores
4. **Deduplication**: Known blocks are sent as 8-byte hash references instead of 4096 bytes
5. **Delta Encoding**: Partially matching blocks are sent as XOR deltas
6. **Raw Fallback**: Genuinely new blocks are sent in full — no worse than normal transfer

The result: for files with repeated blocks (VM images, databases, log files,
storage replication), bandwidth drops dramatically. For fully random data,
it degrades gracefully to raw transfer speed with minimal overhead.

## Requirements

- Two Ubuntu 24.04 machines connected via Ethernet
- Go 1.22+ (the install script will install it if missing)
- Root/sudo access (for installation only)

## Quick Start

### 1. Copy to Both Machines

```bash
scp -r telepathi/ user@MACHINE_IP:~/telepathi/
```

### 2. Build and Install (on BOTH machines)

```bash
cd ~/telepathi
chmod +x install.sh
./install.sh
```

> **Upgrading from v1.0.0:** install the new build on both machines. A v1.1.0
> receiver still accepts files from a v1.0.0 sender, but a v1.0.0 receiver will
> report a failed integrity check for files from a v1.1.0 sender, because the
> checksum now arrives at the end of the transfer.

### 3. Test It

**On Machine A (Receiver):**
```bash
mkdir -p ~/received
telepathi listen -port 7331 -dir ~/received
```

**On Machine B (Sender):**
```bash
telepathi send -host <MACHINE_A_IP> -port 7331 /path/to/file
```

### 4. Tune Parallelism

```bash
# Use 8 workers for hashing and compression (defaults to CPU count)
telepathi send -host <IP> -workers 8 myfile.dat
```

## Generating Test Data

```bash
# 1. ALL ZEROS — maximum dedup (should see ~99% cache hits)
dd if=/dev/zero bs=1M count=1024 of=test_zeros.dat

# 2. REPEATED 4KB BLOCKS — high dedup
dd if=/dev/urandom bs=4096 count=1 of=/tmp/block.dat
for i in $(seq 1 262144); do cat /tmp/block.dat; done > test_repeat.dat

# 3. MIXED — 70% repeated, 30% unique
python3 -c "
import os
block = os.urandom(4096)
with open('test_mixed.dat','wb') as f:
    for i in range(262144):
        if i % 10 < 7:
            f.write(block)
        else:
            f.write(os.urandom(4096))
"

# 4. PURE RANDOM — no dedup possible (baseline)
dd if=/dev/urandom bs=1M count=1024 of=test_random.dat
```

## Understanding the Output

```
╔══════════════════════════════════════════════════════════╗
║            TELEPATHI PROTOCOL - TRANSFER REPORT         ║
╠══════════════════════════════════════════════════════════╣
║  File size (raw):                1.0 GB                 ║
║  Wire bytes (payload):         102.4 MB                 ║
║  Bandwidth savings:              90.0%                  ║
╠══════════════════════════════════════════════════════════╣
║  Total blocks:                 262144                   ║
║  Hash-only (cache hit):        235930  ( 90.0%)         ║
║  Delta (partial match):            0  (  0.0%)          ║
║  Raw (new block):               26214  ( 10.0%)         ║
╠══════════════════════════════════════════════════════════╣
║  Transfer time:                  12.50s                 ║
║  Throughput:                    81.9 MB/s               ║
╠══════════════════════════════════════════════════════════╣
║  Integrity check:              ✓ PASSED                ║
╚══════════════════════════════════════════════════════════╝
```

Key metrics:
- **Hash-only blocks**: Blocks the receiver already had — only an 8-byte hash was sent
- **Raw blocks**: Genuinely new blocks that had to be sent in full
- **Bandwidth savings**: Overall reduction vs sending the raw file

## Architecture

```
telepathi/
├── cmd/telepathi/
│   └── main.go              # CLI: listen/send with pipelined I/O
├── pkg/
│   ├── protocol/
│   │   └── protocol.go      # Quaternary symbols, seeds, framing
│   ├── predictor/
│   │   └── predictor.go     # Block-level cache and sequence prediction
│   ├── engine/
│   │   └── engine.go        # Parallel encoder/decoder with workers
│   └── transport/
│       └── transport.go     # Buffered TCP with framing
├── install.sh
├── go.mod
└── README.md
```

## Firewall

```bash
sudo ufw allow 7331/tcp
```
