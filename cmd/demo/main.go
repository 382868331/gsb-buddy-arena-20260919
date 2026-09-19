// 演示：正常分配/读写/迁移，以及两个真实触发的失败
// （陈旧句柄访问、碎片导致的分配失败）。
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/382868331/gsb-buddy-arena-20260919/buddy"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "演示内部错误:", err)
		os.Exit(1)
	}
}

func main() {
	fmt.Println("=== 伙伴分配器演示（arena = 1 KiB，最小块 16 B）===")
	a, err := buddy.New(10)
	must(err)

	// 1. 正常分配与读写
	h, err := a.Alloc(100, 16)
	must(err)
	off, _ := a.Offset(h)
	fmt.Printf("[正常] Alloc(100, 16) -> 句柄{槽:%d 世代:%d} 偏移:%d\n", h.Slot(), h.Generation(), off)
	msg := []byte("hello, buddy allocator!")
	must(a.Write(h, 0, msg))
	buf := make([]byte, len(msg))
	must(a.Read(h, 0, buf))
	fmt.Printf("[正常] 写入并读回: %q\n", buf)

	// 2. 占住伙伴，迫使增长时迁移
	blocker, err := a.Alloc(128, 16)
	must(err)
	boff, _ := a.Offset(blocker)
	fmt.Printf("[正常] Alloc(128, 16) 占住伙伴块 @%d，阻止原地增长\n", boff)

	pattern := bytes.Repeat([]byte{0x41}, 100)
	must(a.Write(h, 0, pattern))
	h2, err := a.Resize(h, 200)
	must(err)
	noff, _ := a.Offset(h2)
	fmt.Printf("[迁移] Resize(100 -> 200): 偏移 %d -> %d，世代 %d -> %d\n",
		off, noff, h.Generation(), h2.Generation())
	back := make([]byte, 200)
	must(a.Read(h2, 0, back))
	fmt.Printf("[迁移] 前 100 字节保留: %v，增长部分(100..200)全零: %v\n",
		bytes.Equal(back[:100], pattern), bytes.Equal(back[100:], make([]byte, 100)))

	// 3. 真实失败一：陈旧句柄访问
	if err := a.Read(h, 0, buf); err != nil {
		fmt.Printf("[失败] 旧句柄读已被拒绝: %v\n", err)
	}
	if err := a.Free(h); err != nil {
		fmt.Printf("[失败] 旧句柄释放已被拒绝: %v\n", err)
	}

	// 4. 真实失败二：碎片导致分配失败，且状态不变
	small, err := buddy.New(8) // 256 B
	must(err)
	var fills []buddy.Handle
	for i := 0; i < 4; i++ {
		fh, err := small.Alloc(64, 16)
		must(err)
		fills = append(fills, fh)
	}
	must(small.Free(fills[0])) // 释放 @0
	must(small.Free(fills[2])) // 释放 @128（与 @0 不是伙伴）
	freeBefore := small.FreeBytes()
	_, err = small.Alloc(128, 16)
	fmt.Printf("[失败] 空闲 %d B（两个互不为伙伴的 64 B 块），Alloc(128) -> %v\n", freeBefore, err)
	fmt.Printf("[原子性] 失败后空闲字节不变: %v，审计通过: %v\n",
		small.FreeBytes() == freeBefore, small.Audit() == nil)

	// 5. 全局审计
	must(a.Audit())
	must(small.Audit())
	fmt.Printf("[审计] 两个 arena 均满足：块不交叠、完整覆盖、无可合并伙伴遗留\n")
	fmt.Println("=== 演示结束 ===")
}
