package acquisition

import (
	"math"
	"testing"
)

func TestScale(t *testing.T) {
	cases := []struct {
		raw      uint16
		min, max float64
		want     float64
	}{
		{0, 0, 100, 0},
		{65535, 0, 100, 100},
		{32768, 0, 100, 50.0008},
		{32768, -50, 50, 0.0008},
		{65535, 4, 20, 20},
	}
	for _, c := range cases {
		got := Scale(c.raw, c.min, c.max)
		if math.Abs(got-c.want) > 0.001 {
			t.Errorf("Scale(%d,%v,%v) = %v, want %v", c.raw, c.min, c.max, got, c.want)
		}
	}
}
