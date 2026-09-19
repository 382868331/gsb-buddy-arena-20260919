package buddy

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

func mustNew(t *testing.T, p int) *Arena {
	t.Helper()
	a, err := New(p)
	if err != nil {
		t.Fatalf("New(%d): %v", p, err)
	}
	return a
}

func mustAlloc(t *testing.T, a *Arena, size, align int) Handle {
	t.Helper()
	h, err := a.Alloc(size, align)
	if err != nil {
		t.Fatalf("Alloc(%d, %d): %v", size, align, err)
	}
	return h
}

func mustOffset(t *testing.T, a *Arena, h Handle) int {
	t.Helper()
	off, err := a.Offset(h)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	return off
}

func readAll(t *testing.T, a *Arena, h Handle) []byte {
	t.Helper()
	n, err := a.Size(h)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	buf := make([]byte, n)
	if _, err := a.Read(h, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return buf
}

func mustAudit(t *testing.T, a *Arena) {
	t.Helper()
	if err := a.Audit(); err != nil {
		t.Fatalf("Audit: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	for _, p := range []int{3, 0, -1, 25, 100} {
		if _, err := New(p); err == nil {
			t.Fatalf("New(%d) should fail", p)
		}
	}
	for _, p := range []int{4, 7, 24} {
		a, err := New(p)
		if err != nil {
			t.Fatalf("New(%d): %v", p, err)
		}
		if a.Capacity() != 1<<p {
			t.Fatalf("capacity = %d, want %d", a.Capacity(), 1<<p)
		}
		mustAudit(t, a)
	}
}

func TestAlignment(t *testing.T) {
	a := mustNew(t, 8) // 256 bytes
	// Occupy offset 0 with a 16-byte block so the aligned block must move.
	h0 := mustAlloc(t, a, 16, 1)
	h1 := mustAlloc(t, a, 10, 64)
	if off := mustOffset(t, a, h1); off%64 != 0 {
		t.Fatalf("offset %d not 64-aligned", off)
	}
	h2 := mustAlloc(t, a, 1, 128)
	if off := mustOffset(t, a, h2); off%128 != 0 {
		t.Fatalf("offset %d not 128-aligned", off)
	}
	mustAudit(t, a)

	// Invalid alignments.
	for _, al := range []int{0, -2, 3, 24, 512, 1024} {
		if _, err := a.Alloc(8, al); err != ErrInvalidAlignment {
			t.Fatalf("Alloc(8, %d) err = %v, want ErrInvalidAlignment", al, err)
		}
	}
	// Alignment equal to capacity is legal.
	a2 := mustNew(t, 8)
	ha := mustAlloc(t, a2, 1, 256)
	if off := mustOffset(t, a2, ha); off != 0 {
		t.Fatalf("offset = %d, want 0", off)
	}
	_ = h0
}

func TestSplitAndMerge(t *testing.T) {
	a := mustNew(t, 7) // 128 bytes
	h1 := mustAlloc(t, a, 16, 1)
	h2 := mustAlloc(t, a, 16, 1)
	if off := mustOffset(t, a, h1); off != 0 {
		t.Fatalf("first alloc offset = %d, want 0", off)
	}
	if off := mustOffset(t, a, h2); off != 16 {
		t.Fatalf("second alloc offset = %d, want 16", off)
	}
	mustAudit(t, a)
	if err := a.Free(h1); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h2); err != nil {
		t.Fatal(err)
	}
	mustAudit(t, a)
	// Everything merged back: a full-arena block must be allocatable at 0.
	big := mustAlloc(t, a, 100, 1)
	if off := mustOffset(t, a, big); off != 0 {
		t.Fatalf("merged alloc offset = %d, want 0", off)
	}
	mustAudit(t, a)
}

func TestLowestOffsetSameOrder(t *testing.T) {
	a := mustNew(t, 7)
	var hs []Handle
	for i := 0; i < 8; i++ {
		hs = append(hs, mustAlloc(t, a, 16, 1))
	}
	// Free offsets 64 and 16; next alloc must pick 16.
	if err := a.Free(hs[4]); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(hs[1]); err != nil {
		t.Fatal(err)
	}
	h := mustAlloc(t, a, 16, 1)
	if off := mustOffset(t, a, h); off != 16 {
		t.Fatalf("offset = %d, want lowest free 16", off)
	}
	mustAudit(t, a)
}

func TestFragmentationFailure(t *testing.T) {
	a := mustNew(t, 7) // 128 bytes, 8 x 16-byte blocks
	var hs []Handle
	for i := 0; i < 8; i++ {
		hs = append(hs, mustAlloc(t, a, 16, 1))
	}
	// Free alternating blocks: 4 free 16-byte blocks, none adjacent.
	for i := 0; i < 8; i += 2 {
		if err := a.Free(hs[i]); err != nil {
			t.Fatal(err)
		}
	}
	mustAudit(t, a)
	// 64 bytes free in total, but no contiguous 32-byte block exists.
	if _, err := a.Alloc(32, 1); err != ErrNoMemory {
		t.Fatalf("Alloc(32) err = %v, want ErrNoMemory", err)
	}
	// Failure must not change visible state: audit passes and survivors
	// still hold their data.
	mustAudit(t, a)
	for i := 1; i < 8; i += 2 {
		if err := a.Write(hs[i], []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Alloc(32, 1); err != ErrNoMemory {
		t.Fatalf("Alloc(32) err = %v, want ErrNoMemory", err)
	}
	mustAudit(t, a)
	for i := 1; i < 8; i += 2 {
		got := readAll(t, a, hs[i])
		if got[0] != byte(i) {
			t.Fatalf("block %d data corrupted after failed alloc", i)
		}
		for j := 1; j < 16; j++ {
			if got[j] != 0 {
				t.Fatalf("block %d byte %d nonzero after failed alloc", i, j)
			}
		}
	}
}

func TestDoubleFreeAndStaleHandle(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 16, 1)
	if err := a.Write(h, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h); err != ErrStaleHandle {
		t.Fatalf("double free err = %v, want ErrStaleHandle", err)
	}
	if _, err := a.Read(h, make([]byte, 4)); err != ErrStaleHandle {
		t.Fatalf("stale read err = %v, want ErrStaleHandle", err)
	}
	if err := a.Write(h, []byte("x")); err != ErrStaleHandle {
		t.Fatalf("stale write err = %v, want ErrStaleHandle", err)
	}
	if _, err := a.Resize(h, 32); err != ErrStaleHandle {
		t.Fatalf("stale resize err = %v, want ErrStaleHandle", err)
	}
	// Reuse the address and slot: the old handle must stay dead.
	h2 := mustAlloc(t, a, 16, 1)
	if h2.Slot() != h.Slot() {
		t.Fatalf("expected slot reuse, got %d vs %d", h2.Slot(), h.Slot())
	}
	if h2.Gen() == h.Gen() {
		t.Fatal("generation did not advance")
	}
	if off := mustOffset(t, a, h2); off != 0 {
		t.Fatalf("address not reused, offset = %d", off)
	}
	if _, err := a.Read(h, make([]byte, 4)); err != ErrStaleHandle {
		t.Fatalf("old handle revived after address reuse: %v", err)
	}
	if err := a.Free(h); err != ErrStaleHandle {
		t.Fatalf("old handle free after reuse err = %v", err)
	}
	// The zero handle is never valid.
	if _, err := a.Read(Handle{}, make([]byte, 1)); err != ErrStaleHandle {
		t.Fatalf("zero handle read err = %v", err)
	}
	mustAudit(t, a)
}

func TestReadNoWritableAlias(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 16, 1)
	if err := a.Write(h, []byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := a.Read(h, buf); err != nil {
		t.Fatal(err)
	}
	// Mutating the read buffer must not affect arena contents.
	for i := range buf {
		buf[i] = 'X'
	}
	got := readAll(t, a, h)
	if string(got) != "abcdefghijklmnop" {
		t.Fatalf("arena data changed via read buffer: %q", got)
	}
	// Out-of-bounds read/write fail without modifying data.
	if _, err := a.Read(h, make([]byte, 17)); err != ErrOutOfBounds {
		t.Fatalf("oversized read err = %v, want ErrOutOfBounds", err)
	}
	if err := a.Write(h, make([]byte, 17)); err != ErrOutOfBounds {
		t.Fatalf("oversized write err = %v, want ErrOutOfBounds", err)
	}
	got = readAll(t, a, h)
	if string(got) != "abcdefghijklmnop" {
		t.Fatalf("data modified by failed write: %q", got)
	}
}

func TestResizePreservesAlignment(t *testing.T) {
	a := mustNew(t, 9)           // 512
	h0 := mustAlloc(t, a, 16, 1) // sit at offset 0 so h lands at 64
	h := mustAlloc(t, a, 10, 64)
	if err := a.Write(h, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	// Force migration: occupy the neighborhood so in-place growth is
	// impossible, then grow.
	h2 := mustAlloc(t, a, 32, 1)
	h3 := mustAlloc(t, a, 64, 1)
	nh, err := a.Resize(h, 100)
	if err != nil {
		t.Fatalf("Resize grow: %v", err)
	}
	if off := mustOffset(t, a, nh); off%64 != 0 {
		t.Fatalf("resized offset %d lost 64-alignment", off)
	}
	got := readAll(t, a, nh)
	if string(got[:10]) != "0123456789" {
		t.Fatalf("data lost on resize: %q", got[:10])
	}
	for i := 10; i < 100; i++ {
		if got[i] != 0 {
			t.Fatalf("grown byte %d not zeroed", i)
		}
	}
	if _, err := a.Read(h, make([]byte, 1)); err != ErrStaleHandle {
		t.Fatalf("old handle after resize err = %v", err)
	}
	_ = h0
	_ = h2
	_ = h3
	mustAudit(t, a)
}

func TestResizeGrowInPlaceKeepsOffset(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 16, 1) // offset 0, buddy 16..32 free
	if err := a.Write(h, bytes.Repeat([]byte{7}, 16)); err != nil {
		t.Fatal(err)
	}
	nh, err := a.Resize(h, 32)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if off := mustOffset(t, a, nh); off != 0 {
		t.Fatalf("in-place growth moved block to %d", off)
	}
	got := readAll(t, a, nh)
	for i := 0; i < 16; i++ {
		if got[i] != 7 {
			t.Fatalf("byte %d lost on in-place grow", i)
		}
	}
	for i := 16; i < 32; i++ {
		if got[i] != 0 {
			t.Fatalf("grown byte %d not zeroed", i)
		}
	}
	mustAudit(t, a)
}

func TestResizeGrowFailureRollback(t *testing.T) {
	a := mustNew(t, 7) // 128
	// Fill the arena completely: A=16, B=16, C=32, D=64.
	ha := mustAlloc(t, a, 16, 1)
	hb := mustAlloc(t, a, 16, 1)
	hc := mustAlloc(t, a, 32, 1)
	hd := mustAlloc(t, a, 64, 1)
	data := []byte("important-data!!")
	if err := a.Write(ha, data); err != nil {
		t.Fatal(err)
	}
	mustAudit(t, a)
	// Growth needs buddy 16..32 (held by hb) or a new 32-block (none free).
	if _, err := a.Resize(ha, 32); err != ErrNoMemory {
		t.Fatalf("Resize err = %v, want ErrNoMemory", err)
	}
	// Old handle, data and free structure unchanged.
	got := readAll(t, a, ha)
	if !bytes.Equal(got, data) {
		t.Fatalf("data changed after failed resize: %q", got)
	}
	if off := mustOffset(t, a, ha); off != 0 {
		t.Fatalf("offset changed after failed resize: %d", off)
	}
	mustAudit(t, a)
	// The arena is still exactly as full: freeing hb makes room again.
	if err := a.Free(hb); err != nil {
		t.Fatal(err)
	}
	nh, err := a.Resize(ha, 32)
	if err != nil {
		t.Fatalf("Resize after freeing buddy: %v", err)
	}
	if off := mustOffset(t, a, nh); off != 0 {
		t.Fatalf("expected in-place growth at 0, got %d", off)
	}
	_ = hc
	_ = hd
	mustAudit(t, a)
}

func TestResizeShrink(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 64, 1)
	full := make([]byte, 64)
	for i := range full {
		full[i] = byte(i)
	}
	if err := a.Write(h, full); err != nil {
		t.Fatal(err)
	}
	nh, err := a.Resize(h, 16)
	if err != nil {
		t.Fatalf("Resize shrink: %v", err)
	}
	if off := mustOffset(t, a, nh); off != 0 {
		t.Fatalf("shrink moved block to %d", off)
	}
	got := readAll(t, a, nh)
	if !bytes.Equal(got, full[:16]) {
		t.Fatalf("shrink lost prefix: %v", got)
	}
	if _, err := a.Read(h, make([]byte, 1)); err != ErrStaleHandle {
		t.Fatalf("old handle after shrink err = %v", err)
	}
	mustAudit(t, a)
	// The freed buddies are reusable: 48 more bytes fit next to us.
	h2 := mustAlloc(t, a, 48, 1)
	if off := mustOffset(t, a, h2); off != 64 {
		t.Fatalf("expected reuse of freed region, offset = %d", off)
	}
	mustAudit(t, a)
}

func TestZeroing(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 16, 1)
	if err := a.Write(h, bytes.Repeat([]byte{0xFF}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h); err != nil {
		t.Fatal(err)
	}
	// Reallocated block (same address) must be zeroed.
	h2 := mustAlloc(t, a, 16, 1)
	if off := mustOffset(t, a, h2); off != 0 {
		t.Fatalf("offset = %d, want 0", off)
	}
	if got := readAll(t, a, h2); !bytes.Equal(got, make([]byte, 16)) {
		t.Fatalf("reallocated block not zeroed: %v", got)
	}
	// Growth region must be zeroed even across migration.
	if err := a.Write(h2, bytes.Repeat([]byte{0xAA}, 16)); err != nil {
		t.Fatal(err)
	}
	nh, err := a.Resize(h2, 64)
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, a, nh)
	for i := 0; i < 16; i++ {
		if got[i] != 0xAA {
			t.Fatalf("byte %d lost", i)
		}
	}
	for i := 16; i < 64; i++ {
		if got[i] != 0 {
			t.Fatalf("grown byte %d = %#x, want 0", i, got[i])
		}
	}
	mustAudit(t, a)
}

func TestGenerationExhaustion(t *testing.T) {
	a := mustNew(t, 7)
	h := mustAlloc(t, a, 16, 1)
	a.slots[h.Slot()].gen = math.MaxUint64
	h = Handle{slot: h.Slot(), gen: math.MaxUint64}
	if err := a.Free(h); err != ErrGenerationExhausted {
		t.Fatalf("Free err = %v, want ErrGenerationExhausted", err)
	}
	if _, err := a.Resize(h, 32); err != ErrGenerationExhausted {
		t.Fatalf("Resize err = %v, want ErrGenerationExhausted", err)
	}
	// The block is still live and readable.
	if _, err := a.Read(h, make([]byte, 4)); err != nil {
		t.Fatalf("read after exhaustion error: %v", err)
	}
	mustAudit(t, a)
}

// TestReferenceModel runs a fixed-seed random operation sequence against a
// byte-exact reference model, checking contents after every step and the
// atomicity of every failure.
func TestReferenceModel(t *testing.T) {
	const (
		p     = 8 // 256-byte pool: small enough to force failures
		steps = 800
		seed  = 20260919
		maxSz = 64
	)
	rng := rand.New(rand.NewSource(seed))
	a := mustNew(t, p)

	type live struct {
		h     Handle
		data  []byte
		align int
	}
	var alive []live
	var dead []Handle

	alignments := []int{1, 2, 4, 8, 16, 32, 64}

	// checkAll verifies every live handle's contents byte by byte.
	checkAll := func(step int) {
		t.Helper()
		for i, l := range alive {
			buf := make([]byte, len(l.data))
			if _, err := a.Read(l.h, buf); err != nil {
				t.Fatalf("step %d: read live handle %d: %v", step, i, err)
			}
			if !bytes.Equal(buf, l.data) {
				t.Fatalf("step %d: handle %d contents mismatch", step, i)
			}
			off, err := a.Offset(l.h)
			if err != nil {
				t.Fatalf("step %d: offset: %v", step, err)
			}
			if off%l.align != 0 {
				t.Fatalf("step %d: offset %d violates alignment %d", step, off, l.align)
			}
		}
		if err := a.Audit(); err != nil {
			t.Fatalf("step %d: audit: %v", step, err)
		}
	}

	for step := 0; step < steps; step++ {
		op := rng.Intn(6)
		switch {
		case op <= 1 || len(alive) == 0: // alloc
			size := 1 + rng.Intn(maxSz)
			align := alignments[rng.Intn(len(alignments))]
			h, err := a.Alloc(size, align)
			if err != nil {
				if err != ErrNoMemory {
					t.Fatalf("step %d: alloc err = %v", step, err)
				}
				checkAll(step) // failure atomicity
				continue
			}
			data := make([]byte, size)
			if _, err := rng.Read(data); err != nil {
				t.Fatal(err)
			}
			if err := a.Write(h, data); err != nil {
				t.Fatalf("step %d: write after alloc: %v", step, err)
			}
			alive = append(alive, live{h: h, data: data, align: align})

		case op == 2: // free a live handle, or replay a dead one
			if len(dead) > 0 && rng.Intn(3) == 0 {
				d := dead[rng.Intn(len(dead))]
				if err := a.Free(d); err != ErrStaleHandle {
					t.Fatalf("step %d: free of stale handle err = %v", step, err)
				}
				checkAll(step)
				continue
			}
			i := rng.Intn(len(alive))
			if err := a.Free(alive[i].h); err != nil {
				t.Fatalf("step %d: free: %v", step, err)
			}
			dead = append(dead, alive[i].h)
			alive = append(alive[:i], alive[i+1:]...)

		case op == 3: // random partial write + full readback
			i := rng.Intn(len(alive))
			n := 1 + rng.Intn(len(alive[i].data))
			patch := make([]byte, n)
			if _, err := rng.Read(patch); err != nil {
				t.Fatal(err)
			}
			if err := a.Write(alive[i].h, patch); err != nil {
				t.Fatalf("step %d: write: %v", step, err)
			}
			copy(alive[i].data, patch)

		case op == 4: // resize
			i := rng.Intn(len(alive))
			newSize := 1 + rng.Intn(maxSz)
			nh, err := a.Resize(alive[i].h, newSize)
			if err != nil {
				if err != ErrNoMemory {
					t.Fatalf("step %d: resize err = %v", step, err)
				}
				checkAll(step) // rollback: old handle and data intact
				continue
			}
			old := alive[i].data
			nd := make([]byte, newSize)
			copy(nd, old) // keeps min(old, new), zero-pads growth
			dead = append(dead, alive[i].h)
			alive[i] = live{h: nh, data: nd, align: alive[i].align}

		default: // stale handles must never revive
			if len(dead) == 0 {
				continue
			}
			d := dead[rng.Intn(len(dead))]
			if _, err := a.Read(d, make([]byte, 1)); err != ErrStaleHandle {
				t.Fatalf("step %d: stale read err = %v", step, err)
			}
			if err := a.Write(d, []byte{1}); err != ErrStaleHandle {
				t.Fatalf("step %d: stale write err = %v", step, err)
			}
		}
		checkAll(step)
	}
	t.Logf("completed %d steps, %d live, %d dead handles", steps, len(alive), len(dead))
}
