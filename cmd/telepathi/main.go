package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/telepathi/pkg/engine"
	"github.com/telepathi/pkg/gpuaccel"
	"github.com/telepathi/pkg/predictor"
	"github.com/telepathi/pkg/protocol"
	"github.com/telepathi/pkg/transport"
)

const (
	// ChunkSize is how many blocks we process at once before sending
	// 256 blocks * 4KB = 1MB per network message
	BlocksPerChunk = 256
	Version        = "1.1.0"
	DefaultPort    = "7331"

	// PipelineDepth is how many chunks may be in flight between the
	// read, encode and send stages (and recv, decode and write stages).
	PipelineDepth = 4
)

// fileEnd is the MsgFileEnd payload. The sender hashes the file while
// streaming it, so the checksum is only known once the last chunk is sent.
type fileEnd struct {
	Checksum string `json:"checksum"`
}

// safeFileName reduces a peer-supplied name to a single path element so a
// received file can never be written outside the save directory.
func safeFileName(name string) (string, error) {
	base := filepath.Base(filepath.Clean("/" + name))
	if base == "/" || base == "." || base == ".." || base == "" {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	return base, nil
}

type Stats struct {
	RawBytes         uint64  `json:"raw_bytes"`
	WireBytes        uint64  `json:"wire_bytes"`
	HashOnlyBlocks   uint64  `json:"hash_only_blocks"`
	CompressedBlocks uint64  `json:"compressed_blocks"`
	RawBlocks        uint64  `json:"raw_blocks"`
	TotalBlocks      uint64  `json:"total_blocks"`
	HitRate          float64 `json:"hit_rate"`
	TransferTime     float64 `json:"transfer_time_sec"`
	Throughput       float64 `json:"throughput_mbps"`
	Savings          float64 `json:"savings_pct"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "listen":
		cmdListen(os.Args[2:])
	case "send":
		cmdSend(os.Args[2:])
	case "version":
		fmt.Printf("Telepathi Protocol v%s\n", Version)
	case "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Telepathi Protocol - Predictive Block-Level File Transfer

Usage:
  telepathi listen [options]          Start a receiver
  telepathi send [options] <file>     Send a file
  telepathi version                   Show version
  telepathi help                      Show this help

Receiver (listen):
  telepathi listen -port 7331 -dir ./received

  -port string    Port to listen on (default "7331")
  -dir  string    Directory to save received files (default ".")

Sender (send):
  telepathi send -host 192.168.1.10 -port 7331 file.dat

  -host string    Receiver IP address (required)
  -port string    Receiver port (default "7331")
  -workers int    Parallel hash workers (default: CPU count)`)
}

// ========== LISTEN COMMAND ==========

func cmdListen(args []string) {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	port := fs.String("port", DefaultPort, "Port to listen on")
	dir := fs.String("dir", ".", "Directory to save received files")
	fs.Parse(args)

	addr := "0.0.0.0:" + *port
	ln, err := transport.Listen(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	fmt.Printf("Telepathi Protocol v%s\n", Version)
	fmt.Printf("Listening on %s (workers: %d)\n", addr, runtime.NumCPU())
	fmt.Printf("Save directory: %s\n", *dir)
	fmt.Println("Waiting for connections...")

	for {
		rawConn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Accept error: %v\n", err)
			continue
		}

		conn := transport.NewConn(rawConn)
		fmt.Printf("\nConnection from %s\n", conn.RemoteAddr())
		handleReceive(conn, *dir)
		conn.Close()
	}
}

func handleReceive(conn *transport.Conn, dir string) {
	// Step 1: Handshake
	msgType, data, err := conn.RecvMessage()
	if err != nil || msgType != transport.MsgHandshake {
		fmt.Fprintf(os.Stderr, "Handshake failed: %v\n", err)
		return
	}

	var handshake struct {
		Version string `json:"version"`
		Workers int    `json:"workers"`
	}
	json.Unmarshal(data, &handshake)
	fmt.Printf("Peer version: %s (%d workers)\n", handshake.Version, handshake.Workers)

	// Step 2: Seed exchange
	msgType, seedData, err := conn.RecvMessage()
	if err != nil || msgType != transport.MsgSeedExchange {
		fmt.Fprintf(os.Stderr, "Seed exchange failed: %v\n", err)
		return
	}
	var seed protocol.Seed
	copy(seed[:], seedData)
	fmt.Printf("Shared seed: %s...\n", hex.EncodeToString(seed[:8]))

	// Step 3: Initialize predictor with synthetic pre-training
	pred := predictor.NewBlockPredictor()
	pred.PreTrainFromSeed(seed)

	// Send ready
	conn.SendMessage(transport.MsgReady, []byte("ready"))

	// Step 4: File metadata
	msgType, metaData, err := conn.RecvMessage()
	if err != nil || msgType != transport.MsgFileStart {
		fmt.Fprintf(os.Stderr, "File start failed: %v\n", err)
		return
	}

	var fileMeta struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Checksum string `json:"checksum"`
	}
	json.Unmarshal(metaData, &fileMeta)
	fmt.Printf("\nReceiving: %s (%s)\n", fileMeta.Name, humanBytes(uint64(fileMeta.Size)))

	safeName, err := safeFileName(fileMeta.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Rejected file: %v\n", err)
		return
	}
	if safeName != fileMeta.Name {
		fmt.Printf("Saving as: %s\n", safeName)
	}

	outPath := filepath.Join(dir, safeName)
	outFile, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create file: %v\n", err)
		return
	}
	defer outFile.Close()

	hasher := sha256.New()
	var totalRaw uint64
	var wireBytes uint64
	var hashBlocks, compressedBlocks, rawBlocks, totalBlocks uint64
	startTime := time.Now()
	chunks := 0

	// Step 5: Receive, decode and write in three overlapping stages.
	// Decoding stays on this goroutine because the predictor must see
	// blocks in exactly the order the sender produced them.
	recvCh := make(chan []byte, PipelineDepth)
	var recvErr error
	var endChecksum string
	go func() {
		defer close(recvCh)
		for {
			msgType, chunkData, err := conn.RecvMessage()
			if err != nil {
				recvErr = err
				return
			}
			if msgType == transport.MsgFileEnd {
				var fe fileEnd
				if len(chunkData) > 0 {
					json.Unmarshal(chunkData, &fe)
				}
				endChecksum = fe.Checksum
				return
			}
			if msgType == transport.MsgData {
				recvCh <- chunkData
			}
		}
	}()

	writeCh := make(chan []byte, PipelineDepth)
	writeDone := make(chan error, 1)
	go func() {
		var werr error
		for decoded := range writeCh {
			if werr != nil {
				continue // keep draining so the decoder never blocks
			}
			if _, err := outFile.Write(decoded); err != nil {
				werr = err
				continue
			}
			hasher.Write(decoded)
		}
		writeDone <- werr
	}()

	for chunkData := range recvCh {
		wireBytes += uint64(len(chunkData))

		// First 4 bytes: original chunk size
		if len(chunkData) < 4 {
			continue
		}
		origSize := binary.BigEndian.Uint32(chunkData[:4])
		encoded := chunkData[4:]

		// Unmarshal block results
		results, err := engine.UnmarshalResults(encoded)
		if err != nil || results == nil {
			continue
		}

		// Decode blocks
		decoded := engine.DecodeBlocks(results, pred, predictor.BlockSize)

		// Track block types
		for _, r := range results {
			totalBlocks++
			switch r.Type {
			case engine.BlockTypeHash:
				hashBlocks++
			case engine.BlockTypeCompressed:
				compressedBlocks++
			case engine.BlockTypeRaw:
				rawBlocks++
			}
		}

		// Trim to original size
		if uint32(len(decoded)) > origSize {
			decoded = decoded[:origSize]
		}
		totalRaw += uint64(len(decoded))
		writeCh <- decoded

		chunks++
		if chunks%10 == 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				pct := float64(totalRaw) / float64(max64(uint64(fileMeta.Size), 1)) * 100
				hitRate := float64(hashBlocks+compressedBlocks) / float64(max64(totalBlocks, 1)) * 100
				mbps := float64(totalRaw) / 1024 / 1024 / elapsed
				fmt.Printf("\r  Progress: %.1f%% | Dedup+Compress: %.1f%% | %.1f MB/s     ",
					pct, hitRate, mbps)
			}
		}
	}
	close(writeCh)
	writeErr := <-writeDone

	if recvErr != nil {
		fmt.Fprintf(os.Stderr, "\nReceive error: %v\n", recvErr)
		return
	}
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "\nWrite error: %v\n", writeErr)
		return
	}

	// Senders before v1.1.0 put the checksum in the file metadata
	expectedChecksum := endChecksum
	if expectedChecksum == "" {
		expectedChecksum = fileMeta.Checksum
	}

	elapsed := time.Since(startTime)
	outFile.Close()

	// Verify checksum
	gotChecksum := hex.EncodeToString(hasher.Sum(nil))
	checksumOK := expectedChecksum != "" && gotChecksum == expectedChecksum

	savings := (1 - float64(wireBytes)/float64(max64(totalRaw, 1))) * 100

	stats := Stats{
		RawBytes:         totalRaw,
		WireBytes:        wireBytes,
		HashOnlyBlocks:   hashBlocks,
		CompressedBlocks: compressedBlocks,
		RawBlocks:        rawBlocks,
		TotalBlocks:      totalBlocks,
		HitRate:          float64(hashBlocks+compressedBlocks) / float64(max64(totalBlocks, 1)),
		TransferTime:     elapsed.Seconds(),
		Throughput:       float64(totalRaw) / 1024 / 1024 / maxf(elapsed.Seconds(), 0.001),
		Savings:          savings,
	}

	fmt.Print("\n\n")
	printStats(stats, checksumOK)

	statsJSON, _ := json.Marshal(stats)
	conn.SendMessage(transport.MsgStats, statsJSON)
}

// ========== SEND COMMAND ==========

func cmdSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	host := fs.String("host", "", "Receiver IP address")
	port := fs.String("port", DefaultPort, "Receiver port")
	workers := fs.Int("workers", runtime.NumCPU(), "Parallel hash workers")
	fs.Parse(args)

	if *host == "" {
		fmt.Fprintln(os.Stderr, "Error: -host is required")
		os.Exit(1)
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no file specified")
		os.Exit(1)
	}
	filePath := remaining[0]

	file, err := os.Open(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	fileInfo, _ := file.Stat()
	fileSize := fileInfo.Size()
	fileName := filepath.Base(filePath)

	fmt.Printf("Telepathi Protocol v%s\n", Version)
	fmt.Printf("File: %s (%s)\n", fileName, humanBytes(uint64(fileSize)))
	fmt.Printf("Workers: %d\n", *workers)

	// Connect
	addr := *host + ":" + *port
	fmt.Printf("Connecting to %s...\n", addr)
	conn, err := transport.Dial(addr, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("Connected!")

	// Handshake
	hsData, _ := json.Marshal(struct {
		Version string `json:"version"`
		Workers int    `json:"workers"`
	}{Version, *workers})
	conn.SendMessage(transport.MsgHandshake, hsData)

	// Seed
	seed, _ := protocol.GenerateSeed()
	conn.SendMessage(transport.MsgSeedExchange, seed[:])
	fmt.Printf("Shared seed: %s...\n", hex.EncodeToString(seed[:8]))

	// Initialize GPU (optional — falls back to CPU if unavailable)
	gpu := gpuaccel.Init()
	defer gpu.Destroy()
	if gpu.Available() && gpu.IsHardwareGPU() {
		fmt.Printf("GPU: %s (hardware accelerated)\n", gpu.DeviceName())
	} else if gpu.Available() {
		fmt.Printf("GPU: %s (software — using CPU path)\n", gpu.DeviceName())
	} else {
		fmt.Println("GPU: not available (using CPU path)")
	}

	// Initialize predictor
	pred := predictor.NewBlockPredictor()
	pred.PreTrainFromSeed(seed)
	enc := engine.NewParallelEncoder(pred, *workers, gpu)

	// Wait for ready
	msgType, _, err := conn.RecvMessage()
	if err != nil || msgType != transport.MsgReady {
		fmt.Fprintf(os.Stderr, "Peer not ready: %v\n", err)
		os.Exit(1)
	}

	// File metadata (checksum follows in MsgFileEnd, computed while streaming)
	meta, _ := json.Marshal(struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}{fileName, fileSize})
	conn.SendMessage(transport.MsgFileStart, meta)

	fmt.Printf("\nSending...\n")

	// Read, encode and send in three overlapping stages. Encoding stays on
	// this goroutine because the predictor must see blocks in file order.
	chunkSize := BlocksPerChunk * predictor.BlockSize // 1MB
	var totalRaw uint64
	var wireBytes uint64
	chunks := 0
	startTime := time.Now()

	hasher := sha256.New()
	freeBufs := make(chan []byte, PipelineDepth+1)
	for i := 0; i < PipelineDepth+1; i++ {
		freeBufs <- make([]byte, chunkSize)
	}
	stop := make(chan struct{})

	readCh := make(chan []byte, PipelineDepth)
	var readErr error
	go func() {
		defer close(readCh)
		for {
			var buf []byte
			select {
			case buf = <-freeBufs:
			case <-stop:
				return
			}
			n, err := io.ReadFull(file, buf)
			if n > 0 {
				hasher.Write(buf[:n])
				select {
				case readCh <- buf[:n]:
				case <-stop:
					return
				}
			}
			if err != nil {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					readErr = err
				}
				return
			}
		}
	}()

	sendCh := make(chan []byte, PipelineDepth)
	sendDone := make(chan error, 1)
	go func() {
		var serr error
		for frame := range sendCh {
			if serr != nil {
				continue // drain so the encoder never blocks
			}
			if err := conn.SendMessage(transport.MsgData, frame); err != nil {
				serr = err
				close(stop)
			}
		}
		sendDone <- serr
	}()

	for chunk := range readCh {
		totalRaw += uint64(len(chunk))

		results := enc.EncodeChunk(chunk)
		frame := engine.MarshalChunk(uint32(len(chunk)), results)
		freeBufs <- chunk[:cap(chunk)] // results alias chunk; safe once marshaled

		wireBytes += uint64(len(frame))
		sendCh <- frame

		chunks++
		if chunks%5 == 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				pct := float64(totalRaw) / float64(max64(uint64(fileSize), 1)) * 100
				hitRate := float64(enc.HashHits+enc.CompressBlocks) /
					float64(max64(enc.TotalBlocks, 1)) * 100
				mbps := float64(totalRaw) / 1024 / 1024 / elapsed
				fmt.Printf("\r  Progress: %.1f%% | Dedup+Compress: %.1f%% | %.1f MB/s     ",
					pct, hitRate, mbps)
			}
		}
	}
	close(sendCh)
	if err := <-sendDone; err != nil {
		fmt.Fprintf(os.Stderr, "\nSend error: %v\n", err)
		return
	}
	if readErr != nil {
		fmt.Fprintf(os.Stderr, "\nRead error: %v\n", readErr)
		return
	}

	elapsed := time.Since(startTime)
	checksum := hex.EncodeToString(hasher.Sum(nil))
	endData, _ := json.Marshal(fileEnd{Checksum: checksum})
	if err := conn.SendMessage(transport.MsgFileEnd, endData); err != nil {
		fmt.Fprintf(os.Stderr, "\nSend error: %v\n", err)
		return
	}

	// Wait for receiver stats
	msgType, _, err = conn.RecvMessage()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: did not receive receiver stats: %v\n", err)
	}

	savings := (1 - float64(wireBytes)/float64(max64(totalRaw, 1))) * 100

	stats := Stats{
		RawBytes:         totalRaw,
		WireBytes:        wireBytes,
		HashOnlyBlocks:   enc.HashHits,
		CompressedBlocks: enc.CompressBlocks,
		RawBlocks:        enc.RawBlocks,
		TotalBlocks:      enc.TotalBlocks,
		HitRate:          float64(enc.HashHits+enc.CompressBlocks) / float64(max64(enc.TotalBlocks, 1)),
		TransferTime:     elapsed.Seconds(),
		Throughput:       float64(totalRaw) / 1024 / 1024 / maxf(elapsed.Seconds(), 0.001),
		Savings:          savings,
	}

	fmt.Print("\n\n")
	printStats(stats, true)
}

// ========== STATS DISPLAY ==========

func printStats(s Stats, checksumOK bool) {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║            TELEPATHI PROTOCOL - TRANSFER REPORT         ║")
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║  File size (raw):            %12s                ║\n", humanBytes(s.RawBytes))
	fmt.Printf("║  Wire bytes (payload):       %12s                ║\n", humanBytes(s.WireBytes))
	fmt.Printf("║  Bandwidth savings:             %6.1f%%                 ║\n", s.Savings)
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total blocks:               %8d                    ║\n", s.TotalBlocks)
	fmt.Printf("║  Hash-only (dedup hit):      %8d  (%5.1f%%)          ║\n",
		s.HashOnlyBlocks, pct(s.HashOnlyBlocks, s.TotalBlocks))
	fmt.Printf("║  Compressed (new block):     %8d  (%5.1f%%)          ║\n",
		s.CompressedBlocks, pct(s.CompressedBlocks, s.TotalBlocks))
	fmt.Printf("║  Raw (incompressible):       %8d  (%5.1f%%)          ║\n",
		s.RawBlocks, pct(s.RawBlocks, s.TotalBlocks))
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Transfer time:               %7.2fs                  ║\n", s.TransferTime)
	fmt.Printf("║  Throughput:                 %7.1f MB/s               ║\n", s.Throughput)
	fmt.Println("╠══════════════════════════════════════════════════════════╣")
	if checksumOK {
		fmt.Println("║  Integrity check:              ✓ PASSED                ║")
	} else {
		fmt.Println("║  Integrity check:              ✗ FAILED                ║")
	}
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func pct(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}
