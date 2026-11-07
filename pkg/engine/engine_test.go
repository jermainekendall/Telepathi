package engine

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/telepathi/pkg/predictor"
)

func testData(kind string, size int) []byte {
	b := make([]byte, size)
	switch kind {
	case "random":
		rand.Read(b)
	case "text":
		line := []byte("2026-11-07T12:00:00Z INFO request handled path=/api/v1/items status=200 dur=12ms\n")
		for i := range b {
			b[i] = line[i%len(line)] ^ byte(i/4096%7)
		}
	case "mixed":
		rand.Read(b)
		if size < 2*predictor.BlockSize {
			break
		}
		blk := b[:predictor.BlockSize]
		for off := predictor.BlockSize; off+predictor.BlockSize <= size; off += 2 * predictor.BlockSize {
			copy(b[off:], blk)
		}
	}
	return b
}

// Encodes several chunks through a sender-side predictor, decodes them with a
// separate receiver-side predictor, and checks the bytes match exactly.
func TestRoundTrip(t *testing.T) {
	sizes := []int{256 * predictor.BlockSize, 100*predictor.BlockSize + 123, 17}
	for _, kind := range []string{"zeros", "random", "text", "mixed"} {
		for _, size := range sizes {
			seed := [32]byte{1, 2, 3}
			sp, rp := predictor.NewBlockPredictor(), predictor.NewBlockPredictor()
			sp.PreTrainFromSeed(seed)
			rp.PreTrainFromSeed(seed)
			enc := NewParallelEncoder(sp, 4, nil)

			for chunk := 0; chunk < 3; chunk++ {
				data := testData(kind, size)
				orig := append([]byte(nil), data...)
				frame := MarshalChunk(uint32(len(data)), enc.EncodeChunk(data))
				// The sender reuses its read buffer once a chunk is marshaled
				for i := range data {
					data[i] = 0xAA
				}

				results, err := UnmarshalResults(frame[4:])
				if err != nil {
					t.Fatal(err)
				}
				got := DecodeBlocks(results, rp, predictor.BlockSize)
				if len(got) > len(orig) {
					got = got[:len(orig)]
				}
				if !bytes.Equal(got, orig) {
					t.Fatalf("%s size=%d chunk=%d: decoded data differs", kind, size, chunk)
				}
			}
		}
	}
}

func TestHighEntropy(t *testing.T) {
	if !highEntropy(testData("random", predictor.BlockSize)) {
		t.Error("random block should be high entropy")
	}
	if highEntropy(testData("text", predictor.BlockSize)) {
		t.Error("text block should not be high entropy")
	}
	if highEntropy(make([]byte, predictor.BlockSize)) {
		t.Error("zero block should not be high entropy")
	}
}

func benchEncode(b *testing.B, kind string) {
	data := testData(kind, 256*predictor.BlockSize)
	enc := NewParallelEncoder(predictor.NewBlockPredictor(), 4, nil)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh cache contents each iteration without reallocating the map
		if kind == "random" {
			rand.Read(data)
		}
		enc.EncodeChunk(data)
	}
}

func BenchmarkEncodeRandom(b *testing.B) { benchEncode(b, "random") }
func BenchmarkEncodeZeros(b *testing.B)  { benchEncode(b, "zeros") }
func BenchmarkEncodeText(b *testing.B)   { benchEncode(b, "text") }
