package plc

import "testing"

func TestParseAddress(t *testing.T) {
	cases := []struct {
		addr int
		kind RegisterKind
		off  uint16
		ok   bool
	}{
		{10001, DiscreteInput, 0, true},
		{10016, DiscreteInput, 15, true},
		{30001, InputRegister, 0, true},
		{30010, InputRegister, 9, true},
		{40001, HoldingRegister, 0, true},
		{49999, HoldingRegister, 9998, true},
		{10000, 0, 0, false},
		{20001, 0, 0, false},
		{30000, 0, 0, false},
		{40000, 0, 0, false},
		{50001, 0, 0, false},
	}
	for _, c := range cases {
		k, o, err := ParseAddress(c.addr)
		if (err == nil) != c.ok || (c.ok && (k != c.kind || o != c.off)) {
			t.Errorf("ParseAddress(%d) = %v,%d,%v", c.addr, k, o, err)
		}
	}
}

func TestBuildBlocks(t *testing.T) {
	addrs := []int{30002, 30001, 30005, 30050, 40001, 40002, 22222}
	blocks, invalid := BuildBlocks(addrs)

	if len(invalid) != 1 || invalid[6] == nil {
		t.Fatalf("expected index 6 invalid, got %v", invalid)
	}
	want := []Block{
		{InputRegister, 0, 5, []int{1, 0, 2}}, // 30001..30005 in one read
		{InputRegister, 49, 1, []int{3}},      // 30050 too far → own read
		{HoldingRegister, 0, 2, []int{4, 5}},  // 40001..40002
	}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks: %+v", len(blocks), blocks)
	}
	for i, w := range want {
		b := blocks[i]
		if b.Kind != w.Kind || b.Start != w.Start || b.Count != w.Count || len(b.Items) != len(w.Items) {
			t.Errorf("block %d = %+v, want %+v", i, b, w)
		}
	}
}

func TestBuildBlocksDigital(t *testing.T) {
	// DI 10001 and 10060 fit one bit read (gap 58 <= 64); 30001 is a separate kind.
	blocks, _ := BuildBlocks([]int{10060, 30001, 10001})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks: %+v", len(blocks), blocks)
	}
	var di Block
	for _, b := range blocks {
		if b.Kind == DiscreteInput {
			di = b
		}
	}
	if di.Start != 0 || di.Count != 60 || len(di.Items) != 2 {
		t.Errorf("DI block = %+v", di)
	}
}

func TestUnpackBits(t *testing.T) {
	// 0b00000101, 0b00000010 → bits 0,2 and 9 set
	got, err := unpackBits([]byte{0x05, 0x02}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{1, 0, 1, 0, 0, 0, 0, 0, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bit %d = %d, want %d (all %v)", i, got[i], want[i], got)
		}
	}
	if _, err := unpackBits([]byte{0x01}, 10); err == nil {
		t.Error("expected length error")
	}
}
