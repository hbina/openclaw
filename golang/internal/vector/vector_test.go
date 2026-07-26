package vector

import (
	"math"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	input := []float32{0.25, -0.5, 0.75}
	roundTrip, err := Unpack(Pack(input), len(input))
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(roundTrip) != len(input) || roundTrip[1] != input[1] {
		t.Fatalf("round trip = %#v", roundTrip)
	}
}

func TestUnpackRejectsWrongDimensions(t *testing.T) {
	input := []float32{0.25, -0.5, 0.75}
	if _, err := Unpack(Pack(input), 4); err == nil {
		t.Fatal("Unpack accepted wrong dimensions")
	}
}

func TestUnpackRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if _, err := Unpack(Pack([]float32{value}), 1); err == nil {
			t.Fatalf("Unpack accepted non-finite value %v", value)
		}
	}
}

func TestDot(t *testing.T) {
	left := []float32{1, 0, 0}
	right := []float32{0.5, 0.5, 0}
	if got := Dot(left, right); got != 0.5 {
		t.Fatalf("Dot = %v, want 0.5", got)
	}
}
