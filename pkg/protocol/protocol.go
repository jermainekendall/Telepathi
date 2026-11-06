package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Symbol represents a quaternary symbol in the Telepathi Protocol
type Symbol byte

const (
	// Data symbols: represent actual 2-bit data values (used in Markov prediction)
	Sym0 Symbol = 0 // Data bits 00
	Sym1 Symbol = 1 // Data bits 01
	Sym2 Symbol = 2 // Data bits 10
	Sym3 Symbol = 3 // Data bits 11

	// Control symbols: used only in the nudge stream (never in data)
	SymNULL  Symbol = 4 // Prediction correct, no correction needed
	SymERROR Symbol = 5 // Model divergence, resync needed

	// NumDataSymbols is the number of data-plane symbols (for Markov tables)
	NumDataSymbols = 4
)

func (s Symbol) String() string {
	switch s {
	case Sym0:
		return "0"
	case Sym1:
		return "1"
	case Sym2:
		return "2"
	case Sym3:
		return "3"
	case SymNULL:
		return "NULL"
	case SymERROR:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// IsData returns true if the symbol is a data symbol (0-3)
func (s Symbol) IsData() bool {
	return s < NumDataSymbols
}

// BytesToSymbols converts raw bytes to quaternary symbols (2 symbols per byte)
func BytesToSymbols(data []byte) []Symbol {
	symbols := make([]Symbol, len(data)*4)
	for i, b := range data {
		symbols[i*4+0] = Symbol((b >> 6) & 0x03)
		symbols[i*4+1] = Symbol((b >> 4) & 0x03)
		symbols[i*4+2] = Symbol((b >> 2) & 0x03)
		symbols[i*4+3] = Symbol(b & 0x03)
	}
	return symbols
}

// SymbolsToBytes converts quaternary symbols back to raw bytes
func SymbolsToBytes(symbols []Symbol) []byte {
	if len(symbols)%4 != 0 {
		// Pad with Sym0 if needed
		padded := make([]Symbol, len(symbols)+(4-len(symbols)%4))
		copy(padded, symbols)
		symbols = padded
	}
	data := make([]byte, len(symbols)/4)
	for i := range data {
		data[i] = byte(symbols[i*4+0])<<6 |
			byte(symbols[i*4+1])<<4 |
			byte(symbols[i*4+2])<<2 |
			byte(symbols[i*4+3])
	}
	return data
}

// Seed is a 256-bit shared seed
type Seed [32]byte

// GenerateSeed creates a cryptographically random seed
func GenerateSeed() (Seed, error) {
	var s Seed
	_, err := rand.Read(s[:])
	return s, err
}

// DeriveSubSeed deterministically derives a sub-seed for a specific purpose
func (s Seed) DeriveSubSeed(purpose string) Seed {
	h := sha256.New()
	h.Write(s[:])
	h.Write([]byte(purpose))
	var derived Seed
	copy(derived[:], h.Sum(nil))
	return derived
}

// DeriveUint64 derives a deterministic uint64 from the seed
func (s Seed) DeriveUint64(index uint64) uint64 {
	h := sha256.New()
	h.Write(s[:])
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], index)
	h.Write(buf[:])
	sum := h.Sum(nil)
	return binary.LittleEndian.Uint64(sum[:8])
}

// ProfileDescriptor describes the expected data characteristics
type ProfileDescriptor struct {
	Domain     string    `json:"domain"`
	SymbolDist [4]float64 `json:"symbol_dist"` // Expected distribution of ON, OFF, NULL, ERR
	BurstRatio float64   `json:"burst_ratio"`
	BlockSize  uint32    `json:"block_size"`
	WriteRatio float64   `json:"write_ratio"`
	SeqRatio   float64   `json:"seq_ratio"`
}

// DefaultStorageProfile returns a reasonable default for block storage
func DefaultStorageProfile() ProfileDescriptor {
	return ProfileDescriptor{
		Domain:     "block_storage_replication",
		SymbolDist: [4]float64{0.30, 0.30, 0.35, 0.05},
		BurstRatio: 0.40,
		BlockSize:  4096,
		WriteRatio: 0.70,
		SeqRatio:   0.65,
	}
}

// FrameFlag represents frame type flags
type FrameFlag uint8

const (
	FlagDELTA       FrameFlag = 1 << 0
	FlagRESEED      FrameFlag = 1 << 1
	FlagRAW         FrameFlag = 1 << 2
	FlagSCENARIO    FrameFlag = 1 << 3
)

// Frame represents a Telepathi Protocol frame on the wire
type Frame struct {
	TickID  uint32
	Tier    uint8
	Flags   FrameFlag
	Payload []byte
	CRC     uint16
}

// FrameHeaderSize is the fixed header overhead
const FrameHeaderSize = 8 // 4 (tick) + 1 (tier+flags) + 1 (payload len high) + 2 (CRC)

// MarshalFrame serializes a frame to wire format
func MarshalFrame(f *Frame) []byte {
	payloadLen := len(f.Payload)
	buf := make([]byte, 4+1+2+payloadLen+2)

	binary.BigEndian.PutUint32(buf[0:4], f.TickID)
	buf[4] = (f.Tier << 6) | uint8(f.Flags&0x3F)
	binary.BigEndian.PutUint16(buf[5:7], uint16(payloadLen))

	copy(buf[7:7+payloadLen], f.Payload)

	crc := crc32.ChecksumIEEE(buf[:7+payloadLen])
	binary.BigEndian.PutUint16(buf[7+payloadLen:], uint16(crc&0xFFFF))

	return buf
}

// UnmarshalFrame deserializes a frame from wire format
func UnmarshalFrame(data []byte) (*Frame, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	f := &Frame{}
	f.TickID = binary.BigEndian.Uint32(data[0:4])
	f.Tier = data[4] >> 6
	f.Flags = FrameFlag(data[4] & 0x3F)
	payloadLen := binary.BigEndian.Uint16(data[5:7])

	if len(data) < int(7+payloadLen+2) {
		return nil, fmt.Errorf("frame truncated: need %d, have %d", 7+payloadLen+2, len(data))
	}

	f.Payload = make([]byte, payloadLen)
	copy(f.Payload, data[7:7+payloadLen])

	f.CRC = binary.BigEndian.Uint16(data[7+payloadLen:])

	// Verify CRC
	expectedCRC := crc32.ChecksumIEEE(data[:7+payloadLen])
	if uint16(expectedCRC&0xFFFF) != f.CRC {
		return nil, fmt.Errorf("CRC mismatch: expected %04x, got %04x", uint16(expectedCRC&0xFFFF), f.CRC)
	}

	return f, nil
}
