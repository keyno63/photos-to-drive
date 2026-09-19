package main

import (
	"os"
	"syscall"
)

func dataless(s os.FileInfo) bool {
	v, ok := s.Sys().(*syscall.Stat_t)
	return ok && v.Flags&0x40000000 != 0
}
func freeBytes(p string) (uint64, error) {
	var s syscall.Statfs_t
	e := syscall.Statfs(p, &s)
	return s.Bavail * uint64(s.Bsize), e
}
