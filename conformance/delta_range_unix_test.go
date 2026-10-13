//go:build unix

package conformance

import "syscall"

// untouchedBuffer maps n anonymous zero bytes. Nothing reads them in the
// cases that use it, so the pages are never committed: a 4 GiB input costs
// address space only.
func untouchedBuffer(n int) (buf []byte, release func(), err error) {
	buf, err = syscall.Mmap(-1, 0, n, syscall.PROT_READ, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, nil, err
	}
	return buf, func() { _ = syscall.Munmap(buf) }, nil
}
