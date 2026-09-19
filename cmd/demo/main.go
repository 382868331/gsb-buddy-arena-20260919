// Command demo exercises the buddy arena: a normal alloc/write/read/resize
// flow with migration and stale-handle rejection, plus a real fragmentation
// failure. Everything printed is computed by the library at runtime.
package main

import (
	"fmt"
	"os"
	"time"

	buddy "github.com/382868331/gsb-buddy-arena-20260919/buddy"
)

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo: unexpected error:", err)
		os.Exit(1)
	}
}

func main() {
	start := time.Now()
	fmt.Println("=== buddy arena demo (capacity 1 KiB, min block 16 B) ===")

	a, err := buddy.New(10)
	check(err)

	// --- Normal flow -------------------------------------------------
	h, err := a.Alloc(21, 32)
	check(err)
	off, err := a.Offset(h)
	check(err)
	fmt.Printf("[ok] Alloc(21, align 32) -> offset %d (32-aligned: %v)\n", off, off%32 == 0)

	check(a.Write(h, []byte("hello buddy allocator")))
	buf := make([]byte, 21)
	if _, err := a.Read(h, buf); err != nil {
		check(err)
	}
	fmt.Printf("[ok] Read -> %q\n", buf)

	// Occupy the neighbourhood so growth cannot stay in place: the
	// resize must migrate to a new block.
	neighbors := make([]buddy.Handle, 0)
	for i := 0; i < 3; i++ {
		nh, err := a.Alloc(64, 1)
		check(err)
		neighbors = append(neighbors, nh)
	}
	oldOff, err := a.Offset(h)
	check(err)
	h2, err := a.Resize(h, 200)
	check(err)
	newOff, err := a.Offset(h2)
	check(err)
	big := make([]byte, 200)
	if _, err := a.Read(h2, big); err != nil {
		check(err)
	}
	fmt.Printf("[ok] Resize(200) migrated offset %d -> %d, prefix preserved: %q, grown bytes zeroed: %v\n",
		oldOff, newOff, big[:21], allZero(big[21:]))

	// The pre-resize handle is stale now.
	if _, err := a.Read(h, make([]byte, 1)); err != nil {
		fmt.Printf("[ok] stale handle after resize rejected: %v\n", err)
	} else {
		fmt.Fprintln(os.Stderr, "demo: stale handle unexpectedly accepted")
		os.Exit(1)
	}

	// Free and confirm the freed handle stays dead.
	check(a.Free(h2))
	if err := a.Free(h2); err != nil {
		fmt.Printf("[ok] double free rejected: %v\n", err)
	}
	for _, nh := range neighbors {
		check(a.Free(nh))
	}
	check(a.Audit())
	fmt.Println("[ok] audit: blocks tile the arena, no overlap, no mergeable buddies left")

	// --- A real failure: fragmentation -------------------------------
	fmt.Println("--- fragmentation failure ---")
	small, err := buddy.New(7) // 128 bytes = 8 x 16-byte blocks
	check(err)
	blocks := make([]buddy.Handle, 8)
	for i := range blocks {
		blocks[i], err = small.Alloc(16, 1)
		check(err)
	}
	// Free alternating blocks: 64 bytes free, but no contiguous 32 bytes.
	for i := 0; i < 8; i += 2 {
		check(small.Free(blocks[i]))
	}
	_, err = small.Alloc(32, 1)
	if err == nil {
		fmt.Fprintln(os.Stderr, "demo: Alloc(32) unexpectedly succeeded in fragmented arena")
		os.Exit(1)
	}
	fmt.Printf("[fail] Alloc(32) with 64 B free but fragmented -> %v\n", err)
	check(small.Audit())
	fmt.Println("[ok] failed allocation left the arena state consistent (audit passed)")

	fmt.Printf("=== demo finished in %s ===\n", time.Since(start).Round(time.Millisecond))
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
