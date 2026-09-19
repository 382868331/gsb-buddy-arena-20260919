// Package buddy 实现单线程字节 arena 伙伴分配模拟器。
//
// arena 容量为 2^p 字节（4 <= p <= 24），最小块 16 字节。
// 调用方通过带槽号与世代号的 Handle 访问数据；句柄失效后
// （释放或 Resize 成功）任何读写/释放都会失败，地址复用也不会
// 让旧句柄复活。库不暴露底层切片，Read/Write 只做边界内拷贝。
package buddy

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

const (
	// MinP 是允许的最小容量指数（2^4 = 16 字节，恰为最小块）。
	MinP = 4
	// MaxP 是允许的最大容量指数（2^24 = 16 MiB）。
	MaxP = 24
	// MinBlock 是最小块字节数。
	MinBlock = 16

	minOrder = 4 // log2(MinBlock)
)

var (
	ErrInvalidCapacity     = errors.New("buddy: 容量指数必须在 [4, 24] 内")
	ErrInvalidSize         = errors.New("buddy: size 必须为正数")
	ErrInvalidAlignment    = errors.New("buddy: alignment 必须为不超过容量的 2 次幂")
	ErrOutOfMemory         = errors.New("buddy: 没有可满足请求的连续空闲块")
	ErrStaleHandle         = errors.New("buddy: 句柄已失效或不存在")
	ErrOutOfBounds         = errors.New("buddy: 读写越界")
	ErrGenerationExhausted = errors.New("buddy: 句柄世代号已耗尽")
)

// Handle 是访问分配块的凭据，含槽号与世代号。
// 零值 Handle 永远无效。
type Handle struct {
	slot uint32
	gen  uint64
}

// Slot 返回句柄的槽号。
func (h Handle) Slot() uint32 { return h.slot }

// Generation 返回句柄的世代号。
func (h Handle) Generation() uint64 { return h.gen }

type slot struct {
	gen   uint64
	live  bool
	off   int // 块在 arena 内的偏移
	size  int // 逻辑请求大小
	order int // 块阶数，块大小 = 1<<order
	align int // 分配时的对齐
}

// Arena 是固定容量的伙伴分配模拟器，非并发安全。
type Arena struct {
	p         int
	cap       int
	buf       []byte
	free      [][]int // free[order] 为该阶空闲块偏移，升序
	slots     []slot
	freeSlots []uint32
}

// New 创建容量为 2^p 字节的 arena。
func New(p int) (*Arena, error) {
	if p < MinP || p > MaxP {
		return nil, ErrInvalidCapacity
	}
	a := &Arena{
		p:    p,
		cap:  1 << uint(p),
		buf:  make([]byte, 1<<uint(p)),
		free: make([][]int, p+1),
	}
	a.free[p] = []int{0}
	return a, nil
}

// Capacity 返回 arena 总字节数。
func (a *Arena) Capacity() int { return a.cap }

// FreeBytes 返回当前空闲字节总数（可能因碎片无法整体利用）。
func (a *Arena) FreeBytes() int {
	total := 0
	for o := minOrder; o <= a.p; o++ {
		total += len(a.free[o]) << uint(o)
	}
	return total
}

// orderFor 返回容纳 size 的最小阶数（不小于 minOrder）。
func orderFor(size int) int {
	o := minOrder
	for (1 << uint(o)) < size {
		o++
	}
	return o
}

// needOrder 返回同时满足 size 与 alignment 的最小阶数。
func needOrder(size, alignment int) int {
	o := orderFor(size)
	if ao := bits.TrailingZeros(uint(alignment)); ao > o {
		o = ao
	}
	return o
}

func insertSorted(s []int, v int) []int {
	i, _ := slices.BinarySearch(s, v)
	return slices.Insert(s, i, v)
}

func removeSorted(s []int, v int) ([]int, bool) {
	i, ok := slices.BinarySearch(s, v)
	if !ok {
		return s, false
	}
	return slices.Delete(s, i, i+1), true
}

// allocBlock 分配一个 need 阶块并清零，返回偏移；失败时不改变任何状态。
func (a *Arena) allocBlock(need int) (int, bool) {
	k := -1
	for o := need; o <= a.p; o++ {
		if len(a.free[o]) > 0 {
			k = o
			break
		}
	}
	if k < 0 {
		return 0, false
	}
	off := a.free[k][0] // 同阶取最低偏移
	a.free[k] = a.free[k][1:]
	for k > need {
		k--
		a.free[k] = insertSorted(a.free[k], off+(1<<uint(k)))
	}
	clear(a.buf[off : off+(1<<uint(need))])
	return off, true
}

// freeBlockRaw 清零块并递归合并伙伴后挂回空闲表。
func (a *Arena) freeBlockRaw(off, order int) {
	clear(a.buf[off : off+(1<<uint(order))])
	for order < a.p {
		buddy := off ^ (1 << uint(order))
		var ok bool
		a.free[order], ok = removeSorted(a.free[order], buddy)
		if !ok {
			break
		}
		if buddy < off {
			off = buddy
		}
		order++
	}
	a.free[order] = insertSorted(a.free[order], off)
}

// Alloc 分配 size 字节、按 alignment 对齐（相对 arena 偏移 0）的块。
// 失败时不改变任何可见状态。
func (a *Arena) Alloc(size, alignment int) (Handle, error) {
	if size <= 0 {
		return Handle{}, ErrInvalidSize
	}
	if alignment <= 0 || alignment&(alignment-1) != 0 || alignment > a.cap {
		return Handle{}, ErrInvalidAlignment
	}
	need := needOrder(size, alignment)
	if need > a.p {
		return Handle{}, ErrOutOfMemory
	}
	off, ok := a.allocBlock(need)
	if !ok {
		return Handle{}, ErrOutOfMemory
	}
	var si uint32
	if n := len(a.freeSlots); n > 0 {
		si = a.freeSlots[n-1]
		a.freeSlots = a.freeSlots[:n-1]
	} else {
		a.slots = append(a.slots, slot{gen: 1})
		si = uint32(len(a.slots) - 1)
	}
	s := &a.slots[si]
	s.live = true
	s.off = off
	s.size = size
	s.order = need
	s.align = alignment
	return Handle{slot: si, gen: s.gen}, nil
}

func (a *Arena) lookup(h Handle) (*slot, error) {
	if h.slot >= uint32(len(a.slots)) {
		return nil, ErrStaleHandle
	}
	s := &a.slots[h.slot]
	if !s.live || s.gen != h.gen {
		return nil, ErrStaleHandle
	}
	return s, nil
}

// Free 释放句柄对应块，清零并递归合并伙伴。重复释放或陈旧句柄报错。
func (a *Arena) Free(h Handle) error {
	s, err := a.lookup(h)
	if err != nil {
		return err
	}
	if s.gen == math.MaxUint64 {
		return ErrGenerationExhausted
	}
	s.live = false
	s.gen++
	a.freeBlockRaw(s.off, s.order)
	a.freeSlots = append(a.freeSlots, h.slot)
	return nil
}

// Read 把块内 [off, off+len(dst)) 的数据拷贝到 dst。越界失败且不修改 dst。
func (a *Arena) Read(h Handle, off int, dst []byte) error {
	s, err := a.lookup(h)
	if err != nil {
		return err
	}
	if off < 0 || off > s.size || len(dst) > s.size-off {
		return ErrOutOfBounds
	}
	copy(dst, a.buf[s.off+off:s.off+off+len(dst)])
	return nil
}

// Write 把 data 拷贝进块内 [off, off+len(data))。越界失败且不修改任何数据。
func (a *Arena) Write(h Handle, off int, data []byte) error {
	s, err := a.lookup(h)
	if err != nil {
		return err
	}
	if off < 0 || off > s.size || len(data) > s.size-off {
		return ErrOutOfBounds
	}
	copy(a.buf[s.off+off:s.off+off+len(data)], data)
	return nil
}

// Resize 把块调整为 newSize 字节，保留原 alignment。
// 缩小时释放多余伙伴；增长时优先原地合并（保持原偏移），否则迁移。
// 成功返回新句柄并使旧句柄失效；失败时旧句柄、数据与空闲结构全部保留。
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
	need := needOrder(newSize, s.align)
	if need > a.p {
		return Handle{}, ErrOutOfMemory
	}

	switch {
	case need == s.order:
		if newSize > s.size {
			clear(a.buf[s.off+s.size : s.off+newSize])
		}
		s.size = newSize
	case need < s.order: // 缩小：逐阶释放高半块并各自合并
		for o := s.order - 1; o >= need; o-- {
			a.freeBlockRaw(s.off+(1<<uint(o)), o)
		}
		s.order = need
		s.size = newSize
	default: // 增长
		if s.off&((1<<uint(need))-1) == 0 && a.buddiesFree(s.off, s.order, need) {
			for o := s.order; o < need; o++ {
				buddy := s.off + (1 << uint(o))
				a.free[o], _ = removeSorted(a.free[o], buddy)
			}
			clear(a.buf[s.off+s.size : s.off+newSize])
			s.order = need
			s.size = newSize
		} else {
			noff, ok := a.allocBlock(need)
			if !ok {
				return Handle{}, ErrOutOfMemory // 旧句柄/数据/空闲结构不变
			}
			n := min(s.size, newSize)
			copy(a.buf[noff:noff+n], a.buf[s.off:s.off+n])
			a.freeBlockRaw(s.off, s.order)
			s.off = noff
			s.order = need
			s.size = newSize
		}
	}
	s.gen++
	return Handle{slot: h.slot, gen: s.gen}, nil
}

// buddiesFree 检查从 from 阶到 to-1 阶的伙伴是否都空闲（用于原地增长）。
func (a *Arena) buddiesFree(off, from, to int) bool {
	for o := from; o < to; o++ {
		if _, ok := slices.BinarySearch(a.free[o], off+(1<<uint(o))); !ok {
			return false
		}
	}
	return true
}

// Offset 返回块在 arena 内的偏移。
func (a *Arena) Offset(h Handle) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	return s.off, nil
}

// Size 返回块的逻辑请求大小。
func (a *Arena) Size(h Handle) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	return s.size, nil
}

// Alignment 返回块分配时的对齐。
func (a *Arena) Alignment(h Handle) (int, error) {
	s, err := a.lookup(h)
	if err != nil {
		return 0, err
	}
	return s.align, nil
}

// Audit 校验全局不变量：所有已用/空闲块不交叠、完整覆盖 arena、
// 块偏移按阶对齐，且不存在可合并而未合并的同阶伙伴。
func (a *Arena) Audit() error {
	type blk struct {
		off, order int
	}
	var blocks []blk
	freeSet := make(map[uint64]bool)
	for o := minOrder; o <= a.p; o++ {
		for _, off := range a.free[o] {
			key := uint64(off)<<8 | uint64(o)
			if freeSet[key] {
				return fmt.Errorf("buddy: 空闲块重复 off=%d order=%d", off, o)
			}
			freeSet[key] = true
			blocks = append(blocks, blk{off, o})
		}
	}
	for i := range a.slots {
		if s := &a.slots[i]; s.live {
			blocks = append(blocks, blk{s.off, s.order})
		}
	}
	slices.SortFunc(blocks, func(x, y blk) int { return x.off - y.off })
	expect := 0
	for _, b := range blocks {
		if b.order < minOrder || b.order > a.p {
			return fmt.Errorf("buddy: 非法阶数 off=%d order=%d", b.off, b.order)
		}
		if b.off&((1<<uint(b.order))-1) != 0 {
			return fmt.Errorf("buddy: 块未按阶对齐 off=%d order=%d", b.off, b.order)
		}
		if b.off != expect {
			return fmt.Errorf("buddy: 块交叠或有空洞 off=%d 期望=%d", b.off, expect)
		}
		expect += 1 << uint(b.order)
	}
	if expect != a.cap {
		return fmt.Errorf("buddy: 覆盖不完整 expect=%d cap=%d", expect, a.cap)
	}
	for o := minOrder; o < a.p; o++ {
		for _, off := range a.free[o] {
			buddy := off ^ (1 << uint(o))
			if freeSet[uint64(buddy)<<8|uint64(o)] {
				return fmt.Errorf("buddy: 存在未合并伙伴 off=%d order=%d", off, o)
			}
		}
	}
	return nil
}
