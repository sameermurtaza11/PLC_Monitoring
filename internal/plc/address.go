// Package plc handles Modbus TCP details: address conventions,
// grouping registers into block reads, and the TCP client.
// It knows nothing about the database.
package plc

import (
	"fmt"
	"sort"
)

type RegisterKind int

const (
	InputRegister   RegisterKind = iota // 3xxxx → FC04, 16-bit word (analog input)
	HoldingRegister                     // 4xxxx → FC03, 16-bit word
	DiscreteInput                       // 1xxxx → FC02, 1 bit (digital input)
)

func (k RegisterKind) String() string {
	switch k {
	case HoldingRegister:
		return "holding register"
	case DiscreteInput:
		return "discrete input"
	default:
		return "input register"
	}
}

// IsBit reports whether the kind carries a single bit (0/1).
func (k RegisterKind) IsBit() bool { return k == DiscreteInput }

// IsDigital reports whether a pv_metadata address is a 1-bit digital point.
func IsDigital(addr int) bool { return addr >= 10001 && addr <= 19999 }

// ParseAddress converts the 5-digit address used in pv_metadata into a
// register kind and a zero-based Modbus offset:
//
//	10001 → discrete input, offset 0
//	30001 → input,          offset 0
//	30010 → input,          offset 9
//	40001 → holding,        offset 0
func ParseAddress(addr int) (RegisterKind, uint16, error) {
	switch {
	case addr >= 10001 && addr <= 19999:
		return DiscreteInput, uint16(addr - 10001), nil
	case addr >= 30001 && addr <= 39999:
		return InputRegister, uint16(addr - 30001), nil
	case addr >= 40001 && addr <= 49999:
		return HoldingRegister, uint16(addr - 40001), nil
	default:
		return 0, 0, fmt.Errorf("address %d is outside 10001-19999 / 30001-39999 / 40001-49999", addr)
	}
}

// Block is one Modbus read request covering several configured registers.
type Block struct {
	Kind  RegisterKind
	Start uint16 // first offset
	Count uint16 // number of registers
	Items []int  // indices into the caller's address slice
}

// Per-request limits. Bits are cheap (8 per byte), so bit blocks may
// bridge much bigger gaps than 16-bit register blocks.
func limits(k RegisterKind) (maxCount, maxGap int) {
	if k.IsBit() {
		return 2000, 64 // Modbus limit for FC02 is 2000 bits
	}
	return 125, 8 // Modbus limit for FC03/04 is 125 registers
}

// BuildBlocks groups addresses into as few read requests as practical.
// Reading 30001..30010 costs one round-trip instead of ten.
// Invalid addresses are returned separately so the caller can report them.
func BuildBlocks(addresses []int) (blocks []Block, invalid map[int]error) {
	type item struct {
		kind   RegisterKind
		offset uint16
		index  int
	}
	var items []item
	for i, a := range addresses {
		kind, off, err := ParseAddress(a)
		if err != nil {
			if invalid == nil {
				invalid = map[int]error{}
			}
			invalid[i] = err
			continue
		}
		items = append(items, item{kind, off, i})
	}

	sort.Slice(items, func(a, b int) bool {
		if items[a].kind != items[b].kind {
			return items[a].kind < items[b].kind
		}
		return items[a].offset < items[b].offset
	})

	for _, it := range items {
		if n := len(blocks); n > 0 {
			last := &blocks[n-1]
			end := last.Start + last.Count // one past the last register in block
			sameKind := last.Kind == it.kind
			if sameKind && it.offset < end { // duplicate offset: share the read
				last.Items = append(last.Items, it.index)
				continue
			}
			maxCount, maxGap := limits(it.kind)
			if sameKind && int(it.offset)-int(end) <= maxGap &&
				int(it.offset)-int(last.Start)+1 <= maxCount {
				last.Count = it.offset - last.Start + 1
				last.Items = append(last.Items, it.index)
				continue
			}
		}
		blocks = append(blocks, Block{Kind: it.kind, Start: it.offset, Count: 1, Items: []int{it.index}})
	}
	return blocks, invalid
}
