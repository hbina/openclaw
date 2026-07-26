// Package vector provides the shared BLOB encoding and scoring used to store
// and compare embedding vectors across conversation-history recall and
// durable memory search.
package vector

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Pack encodes a float32 embedding vector as a little-endian byte BLOB.
func Pack(vector []float32) []byte {
	data := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(data[index*4:], math.Float32bits(value))
	}
	return data
}

// Unpack decodes a little-endian byte BLOB back into a float32 embedding
// vector, rejecting any non-finite value or a size mismatch against the
// expected dimensions.
func Unpack(data []byte, dimensions int) ([]float32, error) {
	if len(data) != dimensions*4 {
		return nil, fmt.Errorf("embedding BLOB has %d bytes, want %d", len(data), dimensions*4)
	}
	vector := make([]float32, dimensions)
	for index := range vector {
		value := math.Float32frombits(binary.LittleEndian.Uint32(data[index*4:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding BLOB contains non-finite value")
		}
		vector[index] = value
	}
	return vector, nil
}

// Dot returns the dot product of two equal-length vectors. Since Embed
// implementations normalize vectors to unit length, this is equivalent to
// cosine similarity.
func Dot(left, right []float32) float64 {
	var total float64
	for index := range left {
		total += float64(left[index]) * float64(right[index])
	}
	return total
}
