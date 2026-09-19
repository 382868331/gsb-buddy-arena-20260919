// Package buddy implements a single-threaded buddy-allocator simulation
// over a fixed byte arena. Callers access data through handles that carry
// a slot number and a generation counter, so a handle whose block has been
// freed or resized can never observe or corrupt a later allocation that
// happens to reuse the same address or slot.
//
// The arena has capacity 2^p bytes (4 <= p <= 24) and a minimum block size
// of 16 bytes. All alignment is relative to arena offset 0; no real
// pointers and no unsafe code are involved.
package buddy

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// MinBlock is the smallest allocatable block size in bytes.
const MinBlock = 16

// minOrder is log2(MinBlock).
const minOrder = 4

var (
	ErrInvalidCapacity     = errors.New("buddy: capacity exponent p must satisfy 4 <= p <= 24")
	ErrInvalidSize         = errors.New("buddy: size must be positive")
	ErrInvalidAlignment    = errors.New("buddy: alignment must be a power of two not exceeding capacity")
	ErrNoMemory            = errors.New("buddy: allocation failed: no suitable free block")
	ErrStaleHandle         = errors.New("buddy: stale or invalid handle")
	ErrOutOfBounds         = errors.New("buddy: access outside the requested size")
	ErrGenerationExhausted = errors.New("buddy: generation counter exhausted")
)

// Handle identifies a live allocation. It is opaque to callers: copying it
// is fine, but a zero Handle is never valid.
type Handle struct {
	slot int
	gen  uint64
}

// Slot returns the handle's slot index (for diagnostics/tests).
func (h Handle) Slot() int { return h.slot }

// Gen returns the handle's generation (for diagnostics/tests).
func (h Handle) Gen() uint64 { return h.gen }

type slot struct {
	gen       uint64
	live      bool
	offset    int
	size      int // requested size; only data[offset, offset+size) is accessible
	blockSize int // allocated buddy block size (power of two, >= MinBlock)
	alignment int
}

// Arena is a buddy allocator over a fixed byte buffer. Not safe for
// concurrent use; the simulation is single-threaded by design.
type Arena struct {
	p        int
	capacity int
	data     []byte
	free     [][]int // free[order] = sorted ascending offsets of free blocks of size 1<<order
	slots    []slot
}

// New creates an arena of 2^p bytes. p must satisfy 4 <= p <= 24.
func New(p int) (*Arena, error) {
	if p < minOrder || p > 24 {
		return nil, ErrInvalidCapacity
	}
	cap := 1 << p
	a := &Arena{
		p:        p,
		capacity: cap,
		data:     make([]byte, cap),
		free:     make([][]int, p+1),
	}
	a.free[p] = []int{0}
	return a, nil
}

// Capacity returns the arena size in bytes.
func (a *Arena) Capacity() int { return a.capacity }

func log2(n int) int {
	r := 0
	for n > 1 {
		n >>= 1
		r++
	}
	return r
}

// blockFor returns the minimal block size that can hold size bytes while
// respecting alignment. Buddy blocks of size 2^k always sit at offsets that
// are multiples of 2^k, so blockSize >= alignment guarantees alignment.
func blockFor(size, alignment int) int {
	b := MinBlock
	for b < size {
		b <<= 1
	}
	for b < alignment {
		b <<= 1
	}
	return b
}

// allocBlock removes and returns the lowest-offset free block of the given
// order, splitting larger blocks as needed. It reports false without
// mutating any state when no block is available.
func (a *Arena) allocBlock(order int) (int, bool) {
	k := order
	for k <= a.p && len(a.free[k]) == 0 {
		k++
	}
	if k > a.p {
		return 0, false
	}
	off := a.free[k][0] // lowest offset at this order (list is sorted)
	a.free[k] = a.free[k][1:]
	for k > order {
		k--
		a.free[k] = insertSorted(a.free[k], off+(1<<k))
	}
	return off, true
}

// freeBlock inserts the block (off, 1<<order) into the free lists, merging
// recursively with its buddy while the buddy is free.
func (a *Arena) freeBlock(off, order int) {
	for order < a.p {
		buddy := off ^ (1 << order)
		i := sort.SearchInts(a.free[order], buddy)
		if i >= len(a.free[order]) || a.free[order][i] != buddy {
			break
		}
		a.free[order] = append(a.free[order][:i], a.free[order][i+1:]...)
		if buddy < off {
			off = buddy
		}
		order++
	}
	a.free[order] = insertSorted(a.free[order], off)
}

func insertSorted(s []int, v int) []int {
	i := sort.SearchInts(s, v)
	s = append(s, 0)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func (a *Arena) hasFree(order, off int) bool {
	i := sort.SearchInts(a.free[order], off)
	return i < len(a.free[order]) && a.free[order][i] == off
}

func (a *Arena) removeFree(order, off int) {
	i := sort.SearchInts(a.free[order], off)
	a.free[order] = append(a.free[order][:i], a.free[order][i+1:]...)
}

// Alloc reserves a block holding size bytes with the given alignment
// (relative to arena offset 0). alignment must be a power of two not
// exceeding the arena capacity. The block is zeroed before use. On failure
// the arena's visible state is unchanged.
func (a *Arena) Alloc(size, alignment int) (Handle, error) {
	if size <= 0 {
		return Handle{}, ErrInvalidSize
	}
	if alignment < 1 || alignment > a.capacity || alignment&(alignment-1) != 0 {
		return Handle{}, ErrInvalidAlignment
	}
	block := blockFor(size, alignment)
	if block > a.capacity {
		return Handle{}, ErrNoMemory
	}
	off, ok := a.allocBlock(log2(block))
	if !ok {
		return Handle{}, ErrNoMemory
	}
	clear(a.data[off : off+block])

	idx := -1
	for i := range a.slots {
		if !a.slots[i].live {
			idx = i
			break
		}
	}
	if idx == -1 {
		a.slots = append(a.slots, slot{gen: 1})
		idx = len(a.slots) - 1
	}
	s := &a.slots[idx]
	s.live = true
	s.offset = off
	s.size = size
	s.blockSize = block
	s.alignment = alignment
	return Handle{slot: idx, gen: s.gen}, nil
}

func (a *Arena) lookup(h Handle) (*slot, error) {
	if h.slot < 0 || h.slot >= len(a.slots) {
		return nil, ErrStaleHandle
	}
	s := &a.slots[h.slot]
	if !s.live || h.gen == 0 || s.gen != h.gen {
		return nil, ErrStaleHandle
	}
	return s, nil
}

// Free releases the block. The bytes are zeroed and the slot's generation
// is bumped, so the freed handle stays invalid even if the address and slot
// are later reused. Freeing a stale handle fails.
func (a *Arena) Free(h Handle) error {
	s, err := a.lookup(h)
	if err != nil {
		return err
	}
	if s.gen == math.MaxUint64 {
		return ErrGenerationExhausted
	}
	clear(a.data[s.offset : s.offset+s.blockSize])
	s.live = false
	s.gen++
	a.freeBlock(s.offset, log2(s.blockSize))
	return nil
}

// Read copies up to len(buf) bytes from the block into buf. len(buf) must
// not exceed the requested size of the allocation. It never exposes the
// underlying arena storage.
func (a *Arena) Read(h Handle, buf []byte) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	if len(buf) > s.size {
		return 0, ErrOutOfBounds
	}
	return copy(buf, a.data[s.offset:s.offset+len(buf)]), nil
}

// Write copies data into the block. len(data) must not exceed the
// requested size; on violation nothing is modified.
func (a *Arena) Write(h Handle, data []byte) error {
	s, err := a.lookup(h)
	if err != nil {
		return err
	}
	if len(data) > s.size {
		return ErrOutOfBounds
	}
	copy(a.data[s.offset:s.offset+len(data)], data)
	return nil
}

// Size returns the requested size of a live allocation.
func (a *Arena) Size(h Handle) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	return s.size, nil
}

// Offset returns the block's arena offset (a multiple of its alignment).
func (a *Arena) Offset(h Handle) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	return s.offset, nil
}

// Resize changes the allocation to newSize (> 0), preserving the original
// alignment. Shrinking may free buddy blocks; growing merges free buddies
// in place only when the offset can be preserved, otherwise it allocates a
// new block and migrates. On success the first min(old, new) bytes are
// preserved, the grown part is zeroed, and the old handle is invalidated
// (the returned handle replaces it). If growth needs a new block and none
// is available, Resize fails and the old handle, data and free structure
// are untouched.
func (a *Arena) Resize(h Handle, newSize int) (Handle, error) {
	s, err := a.lookup(h)
	if err != nil {
		return Handle{}, err
	}
	if newSize <= 0 {
		return Handle{}, ErrInvalidSize
	}
	if s.gen == math.MaxUint64 {
		return Handle{}, ErrGenerationExhausted
	}
	oldOff, oldSize, oldBlock := s.offset, s.size, s.blockSize
	alignment := s.alignment
	newBlock := blockFor(newSize, alignment)
	if newBlock > a.capacity {
		return Handle{}, ErrNoMemory
	}

	switch {
	case newBlock == oldBlock:
		if newSize < oldSize {
			clear(a.data[oldOff+newSize : oldOff+oldSize])
		}
		s.size = newSize

	case newBlock < oldBlock:
		// Shrink in place: zero the tail, then free the buddy pieces
		// split off from the top of the old block.
		clear(a.data[oldOff+newSize : oldOff+oldBlock])
		for i := log2(oldBlock) - 1; i >= log2(newBlock); i-- {
			a.freeBlock(oldOff+(1<<i), i)
		}
		s.size = newSize
		s.blockSize = newBlock

	default:
		// Grow. In-place merge is allowed only while the offset stays
		// the lower half of each merged pair. Simulate first so a
		// failed attempt leaves no trace.
		target := log2(newBlock)
		ok := true
		for o := log2(oldBlock); o < target; o++ {
			if oldOff%(1<<(o+1)) != 0 || !a.hasFree(o, oldOff+(1<<o)) {
				ok = false
				break
			}
		}
		if ok {
			for o := log2(oldBlock); o < target; o++ {
				a.removeFree(o, oldOff+(1<<o))
			}
			clear(a.data[oldOff+oldSize : oldOff+newBlock])
			s.size = newSize
			s.blockSize = newBlock
		} else {
			// Migrate: allocate first; on failure the old handle,
			// data and free structure remain untouched.
			nh, err := a.Alloc(newSize, alignment)
			if err != nil {
				return Handle{}, err
			}
			ns, err := a.lookup(nh)
			if err != nil {
				return Handle{}, err
			}
			copy(a.data[ns.offset:ns.offset+oldSize], a.data[oldOff:oldOff+oldSize])
			if err := a.Free(h); err != nil {
				return Handle{}, err
			}
			return nh, nil
		}
	}

	s.gen++
	return Handle{slot: h.slot, gen: s.gen}, nil
}

// Audit verifies the global memory-partition invariants: used and free
// blocks are well-formed, do not overlap, and together cover the whole
// arena, and no two free blocks at the same order are mergeable buddies.
func (a *Arena) Audit() error {
	type blk struct{ off, size int }
	var blocks []blk
	for i := range a.slots {
		if a.slots[i].live {
			blocks = append(blocks, blk{a.slots[i].offset, a.slots[i].blockSize})
		}
	}
	for order := minOrder; order <= a.p; order++ {
		for _, off := range a.free[order] {
			blocks = append(blocks, blk{off, 1 << order})
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].off < blocks[j].off })
	expect := 0
	for _, b := range blocks {
		if b.off != expect {
			return fmt.Errorf("buddy: gap or overlap at offset %d (expected %d)", b.off, expect)
		}
		if b.size < MinBlock || b.size&(b.size-1) != 0 {
			return fmt.Errorf("buddy: bad block size %d at offset %d", b.size, b.off)
		}
		if b.off%b.size != 0 {
			return fmt.Errorf("buddy: misaligned block at offset %d size %d", b.off, b.size)
		}
		expect += b.size
	}
	if expect != a.capacity {
		return fmt.Errorf("buddy: blocks cover %d bytes, capacity is %d", expect, a.capacity)
	}
	for order := minOrder; order < a.p; order++ {
		set := make(map[int]bool, len(a.free[order]))
		for _, off := range a.free[order] {
			set[off] = true
		}
		for _, off := range a.free[order] {
			if set[off^(1<<order)] {
				return fmt.Errorf("buddy: mergeable free buddies left at order %d offset %d", order, off)
			}
		}
	}
	return nil
}
