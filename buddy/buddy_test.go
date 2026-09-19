package buddy

import (
	"bytes"
	"errors"
	"math/rand/v2"
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
	if err := a.Read(h, 0, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return buf
}

func audit(t *testing.T, a *Arena) {
	t.Helper()
	if err := a.Audit(); err != nil {
		t.Fatalf("Audit: %v", err)
	}
}

// 对齐：分配偏移必须满足 alignment（相对 arena 偏移 0）。
func TestAlignment(t *testing.T) {
	a := mustNew(t, 8) // 256 B
	h1 := mustAlloc(t, a, 16, 16)
	if off := mustOffset(t, a, h1); off != 0 {
		t.Fatalf("h1 off = %d, want 0", off)
	}
	h2 := mustAlloc(t, a, 1, 64)
	if off := mustOffset(t, a, h2); off != 64 {
		t.Fatalf("h2 off = %d, want 64", off)
	}
	h3 := mustAlloc(t, a, 1, 128)
	if off := mustOffset(t, a, h3); off != 128 {
		t.Fatalf("h3 off = %d, want 128", off)
	}
	if _, err := a.Alloc(1, 0); !errors.Is(err, ErrInvalidAlignment) {
		t.Fatalf("align=0: %v", err)
	}
	if _, err := a.Alloc(1, 3); !errors.Is(err, ErrInvalidAlignment) {
		t.Fatalf("align=3: %v", err)
	}
	if _, err := a.Alloc(1, 512); !errors.Is(err, ErrInvalidAlignment) {
		t.Fatalf("align>cap: %v", err)
	}
	if _, err := a.Alloc(0, 16); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("size=0: %v", err)
	}
	audit(t, a)
}

// 拆分与合并：同阶选最低偏移，释放后递归合并。
func TestSplitMerge(t *testing.T) {
	a := mustNew(t, 6) // 64 B
	hs := make([]Handle, 4)
	for i := range hs {
		hs[i] = mustAlloc(t, a, 16, 16)
		if off := mustOffset(t, a, hs[i]); off != i*16 {
			t.Fatalf("block %d off = %d, want %d", i, off, i*16)
		}
	}
	// 释放偏移 0 的块，再分配应复用最低偏移。
	if err := a.Free(hs[0]); err != nil {
		t.Fatal(err)
	}
	h := mustAlloc(t, a, 16, 16)
	if off := mustOffset(t, a, h); off != 0 {
		t.Fatalf("reuse off = %d, want 0", off)
	}
	// 全部释放后应合并回完整 64 B 块。
	for _, x := range hs[1:] {
		if err := a.Free(x); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Free(h); err != nil {
		t.Fatal(err)
	}
	if got := a.FreeBytes(); got != 64 {
		t.Fatalf("FreeBytes = %d, want 64", got)
	}
	big := mustAlloc(t, a, 64, 64)
	if off := mustOffset(t, a, big); off != 0 {
		t.Fatalf("merged block off = %d, want 0", off)
	}
	audit(t, a)
}

// 碎片失败：有空闲字节但无连续块时分配失败，且可见状态不变。
func TestFragmentationFailure(t *testing.T) {
	a := mustNew(t, 6) // 64 B
	hs := make([]Handle, 4)
	for i := range hs {
		hs[i] = mustAlloc(t, a, 16, 16)
	}
	// 释放偏移 16 与 48 的块：两个 16 B 空闲块互不为伙伴。
	if err := a.Free(hs[1]); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(hs[3]); err != nil {
		t.Fatal(err)
	}
	freeBefore := a.FreeBytes()
	if _, err := a.Alloc(32, 16); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("want ErrOutOfMemory, got %v", err)
	}
	if got := a.FreeBytes(); got != freeBefore {
		t.Fatalf("失败后 FreeBytes = %d, want %d", got, freeBefore)
	}
	// 16 B 分配仍应成功，证明空闲结构未被失败操作破坏。
	h := mustAlloc(t, a, 16, 16)
	if off := mustOffset(t, a, h); off != 16 {
		t.Fatalf("off = %d, want 16", off)
	}
	audit(t, a)
}

// 重复释放：第二次释放必须失败。
func TestDoubleFree(t *testing.T) {
	a := mustNew(t, 6)
	h := mustAlloc(t, a, 16, 16)
	if err := a.Free(h); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("double free: %v", err)
	}
	audit(t, a)
}

// 地址复用不复活旧句柄。
func TestStaleHandleAfterReuse(t *testing.T) {
	a := mustNew(t, 6)
	h1 := mustAlloc(t, a, 16, 16)
	if err := a.Write(h1, 0, bytes.Repeat([]byte{0xAB}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h1); err != nil {
		t.Fatal(err)
	}
	h2 := mustAlloc(t, a, 16, 16)
	if off := mustOffset(t, a, h2); off != 0 { // 同地址复用
		t.Fatalf("复用偏移 = %d, want 0", off)
	}
	if h2 == h1 {
		t.Fatal("世代未推进，新旧句柄相同")
	}
	var buf [16]byte
	if err := a.Read(h1, 0, buf[:]); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale read: %v", err)
	}
	if err := a.Write(h1, 0, buf[:]); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale write: %v", err)
	}
	if err := a.Free(h1); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("stale free: %v", err)
	}
	// 新句柄读到的是清零后的数据。
	if got := readAll(t, a, h2); !bytes.Equal(got, make([]byte, 16)) {
		t.Fatalf("复用块未清零: %x", got)
	}
	audit(t, a)
}

// Read 不泄漏可写别名：修改 Read 的目标缓冲不影响 arena；
// Write 只拷贝，修改源切片不影响 arena。
func TestReadNoWritableAlias(t *testing.T) {
	a := mustNew(t, 6)
	h := mustAlloc(t, a, 16, 16)
	src := []byte("0123456789abcdef")
	if err := a.Write(h, 0, src); err != nil {
		t.Fatal(err)
	}
	src[0] = 'X' // 修改源切片
	buf := make([]byte, 16)
	if err := a.Read(h, 0, buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != '0' {
		t.Fatalf("Write 未拷贝数据，源切片修改泄漏进 arena: %q", buf)
	}
	buf[1] = 'Y' // 修改 Read 目标缓冲
	buf2 := make([]byte, 16)
	if err := a.Read(h, 0, buf2); err != nil {
		t.Fatal(err)
	}
	if buf2[1] != '1' {
		t.Fatalf("Read 泄漏可写别名: %q", buf2)
	}
}

// 越界读写失败且不修改数据。
func TestOutOfBounds(t *testing.T) {
	a := mustNew(t, 6)
	h := mustAlloc(t, a, 16, 16)
	if err := a.Write(h, 0, bytes.Repeat([]byte{7}, 16)); err != nil {
		t.Fatal(err)
	}
	if err := a.Write(h, 8, make([]byte, 9)); !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("write overflow: %v", err)
	}
	if err := a.Write(h, -1, make([]byte, 1)); !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("write negative: %v", err)
	}
	if err := a.Read(h, 15, make([]byte, 2)); !errors.Is(err, ErrOutOfBounds) {
		t.Fatalf("read overflow: %v", err)
	}
	want := bytes.Repeat([]byte{7}, 16)
	if got := readAll(t, a, h); !bytes.Equal(got, want) {
		t.Fatalf("越界写修改了数据: %v", got)
	}
}

// Resize 保留原 alignment（迁移后偏移仍满足对齐）。
func TestResizeKeepsAlignment(t *testing.T) {
	a := mustNew(t, 10)           // 1 KiB
	h := mustAlloc(t, a, 24, 128) // 128 B 块 @0
	if err := a.Write(h, 0, bytes.Repeat([]byte{0x5A}, 24)); err != nil {
		t.Fatal(err)
	}
	blocker := mustAlloc(t, a, 128, 16) // 占住 @128，阻止原地增长
	h2, err := a.Resize(h, 200)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	off := mustOffset(t, a, h2)
	if off%128 != 0 {
		t.Fatalf("迁移后偏移 %d 不满足 alignment=128", off)
	}
	if off == 0 {
		t.Fatalf("伙伴被占用时应迁移，偏移仍为 0")
	}
	got := readAll(t, a, h2)
	if !bytes.Equal(got[:24], bytes.Repeat([]byte{0x5A}, 24)) {
		t.Fatalf("前 24 字节未保留: %x", got[:24])
	}
	if !bytes.Equal(got[24:], make([]byte, 176)) {
		t.Fatalf("增长部分未清零")
	}
	if al, _ := a.Alignment(h2); al != 128 {
		t.Fatalf("alignment = %d, want 128", al)
	}
	_ = blocker
	audit(t, a)
}

// 原地增长：伙伴空闲时保持原偏移。
func TestResizeGrowInPlace(t *testing.T) {
	a := mustNew(t, 8)
	h := mustAlloc(t, a, 16, 16)
	if err := a.Write(h, 0, bytes.Repeat([]byte{3}, 16)); err != nil {
		t.Fatal(err)
	}
	h2, err := a.Resize(h, 32)
	if err != nil {
		t.Fatal(err)
	}
	if off := mustOffset(t, a, h2); off != 0 {
		t.Fatalf("原地增长应保持偏移 0，实际 %d", off)
	}
	got := readAll(t, a, h2)
	if !bytes.Equal(got[:16], bytes.Repeat([]byte{3}, 16)) || !bytes.Equal(got[16:], make([]byte, 16)) {
		t.Fatalf("数据保留/清零错误: %v", got)
	}
	audit(t, a)
}

// 增长失败回滚：无新块可用时旧句柄、数据、空闲结构全部保留。
func TestResizeGrowFailureRollback(t *testing.T) {
	a := mustNew(t, 7)            // 128 B
	ha := mustAlloc(t, a, 16, 16) // @0
	hb := mustAlloc(t, a, 16, 16) // @16，占住伙伴
	hc := mustAlloc(t, a, 32, 16) // @32
	hd := mustAlloc(t, a, 64, 64) // @64，填满
	_, _, _ = hb, hc, hd
	if err := a.Write(ha, 0, bytes.Repeat([]byte{9}, 16)); err != nil {
		t.Fatal(err)
	}
	freeBefore := a.FreeBytes()
	if _, err := a.Resize(ha, 32); !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("want ErrOutOfMemory, got %v", err)
	}
	if got := a.FreeBytes(); got != freeBefore {
		t.Fatalf("失败后 FreeBytes = %d, want %d", got, freeBefore)
	}
	if got := readAll(t, a, ha); !bytes.Equal(got, bytes.Repeat([]byte{9}, 16)) {
		t.Fatalf("旧数据被破坏: %v", got)
	}
	if off := mustOffset(t, a, ha); off != 0 {
		t.Fatalf("旧块偏移变化: %d", off)
	}
	audit(t, a)
}

// 清零：释放块与新分配块内容为零；缩小释放的伙伴也被清零。
func TestZeroing(t *testing.T) {
	a := mustNew(t, 8)
	h := mustAlloc(t, a, 32, 16)
	if err := a.Write(h, 0, bytes.Repeat([]byte{0xFF}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(h); err != nil {
		t.Fatal(err)
	}
	h2 := mustAlloc(t, a, 32, 16)
	if got := readAll(t, a, h2); !bytes.Equal(got, make([]byte, 32)) {
		t.Fatalf("新分配块未清零: %x", got)
	}
	// 缩小后释放的伙伴被清零：写满 64 B，缩到 16 B，再分配 16 B 应读到零。
	big := mustAlloc(t, a, 64, 16)
	if err := a.Write(big, 0, bytes.Repeat([]byte{0xEE}, 64)); err != nil {
		t.Fatal(err)
	}
	small, err := a.Resize(big, 16)
	if err != nil {
		t.Fatal(err)
	}
	off := mustOffset(t, a, small)
	nb := mustAlloc(t, a, 16, 16)
	if noff := mustOffset(t, a, nb); noff != off+16 {
		t.Fatalf("期望复用刚释放的伙伴 @%d，实际 @%d", off+16, noff)
	}
	if got := readAll(t, a, nb); !bytes.Equal(got, make([]byte, 16)) {
		t.Fatalf("缩小释放的伙伴未清零: %x", got)
	}
	audit(t, a)
}

// Resize 成功后旧句柄失效、世代推进。
func TestResizeInvalidatesOldHandle(t *testing.T) {
	a := mustNew(t, 8)
	h := mustAlloc(t, a, 16, 16)
	h2, err := a.Resize(h, 16) // 同阶也算成功
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h {
		t.Fatal("Resize 成功后世代未推进")
	}
	if err := a.Free(h); !errors.Is(err, ErrStaleHandle) {
		t.Fatalf("旧句柄应失效: %v", err)
	}
	if err := a.Free(h2); err != nil {
		t.Fatal(err)
	}
	audit(t, a)
}

// 随机验证：固定种子，小池，与逐字节参考模型对比，
// 每步后审计全局不变量，失败操作后验证失败原子性。
func TestRandomAgainstReferenceModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(0xB0D1, 0xAEA1))
	a := mustNew(t, 8) // 256 B
	model := map[Handle][]byte{}
	var live []Handle

	verifyAll := func() {
		t.Helper()
		for h, want := range model {
			if got := readAll(t, a, h); !bytes.Equal(got, want) {
				t.Fatalf("句柄 %+v 数据不一致: got %x want %x", h, got, want)
			}
		}
	}

	aligns := []int{1, 2, 4, 8, 16, 32, 64, 128}
	for step := 0; step < 3000; step++ {
		switch rng.IntN(4) {
		case 0: // Alloc
			size := 1 + rng.IntN(80)
			al := aligns[rng.IntN(len(aligns))]
			h, err := a.Alloc(size, al)
			if err != nil {
				if !errors.Is(err, ErrOutOfMemory) {
					t.Fatalf("step %d: 意外错误 %v", step, err)
				}
				verifyAll() // 失败原子性
				break
			}
			if off := mustOffset(t, a, h); off%al != 0 {
				t.Fatalf("step %d: 偏移 %d 不满足对齐 %d", step, off, al)
			}
			model[h] = make([]byte, size)
			live = append(live, h)
		case 1: // Free
			if len(live) == 0 {
				continue
			}
			i := rng.IntN(len(live))
			h := live[i]
			live = slicesDelete(live, i)
			if err := a.Free(h); err != nil {
				t.Fatalf("step %d: Free: %v", step, err)
			}
			delete(model, h)
		case 2: // Write + Read 校验
			if len(live) == 0 {
				continue
			}
			h := live[rng.IntN(len(live))]
			data := model[h]
			off := rng.IntN(len(data) + 1)
			n := rng.IntN(len(data) - off + 1)
			chunk := make([]byte, n)
			for i := range chunk {
				chunk[i] = byte(rng.IntN(256))
			}
			if err := a.Write(h, off, chunk); err != nil {
				t.Fatalf("step %d: Write: %v", step, err)
			}
			copy(data[off:], chunk)
			got := readAll(t, a, h)
			if !bytes.Equal(got, data) {
				t.Fatalf("step %d: 写后读回不一致", step)
			}
		case 3: // Resize
			if len(live) == 0 {
				continue
			}
			i := rng.IntN(len(live))
			h := live[i]
			old := model[h]
			newSize := 1 + rng.IntN(80)
			h2, err := a.Resize(h, newSize)
			if err != nil {
				if !errors.Is(err, ErrOutOfMemory) {
					t.Fatalf("step %d: 意外错误 %v", step, err)
				}
				// 回滚校验：旧句柄与数据不变
				if got := readAll(t, a, h); !bytes.Equal(got, old) {
					t.Fatalf("step %d: Resize 失败后旧数据改变", step)
				}
				break
			}
			nd := make([]byte, newSize)
			copy(nd, old)
			delete(model, h)
			model[h2] = nd
			live[i] = h2
			if got := readAll(t, a, h2); !bytes.Equal(got, nd) {
				t.Fatalf("step %d: Resize 后数据不一致", step)
			}
		}
		if err := a.Audit(); err != nil {
			t.Fatalf("step %d: Audit: %v", step, err)
		}
		if step%97 == 0 {
			verifyAll()
		}
	}
	verifyAll()
}

func slicesDelete(s []Handle, i int) []Handle {
	copy(s[i:], s[i+1:])
	return s[:len(s)-1]
}
