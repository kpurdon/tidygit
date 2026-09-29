//go:build darwin || linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func renamePinnedArchive(sourceParent, destinationParent *os.File, name string) error {
	return unix.Renameat(int(sourceParent.Fd()), name, int(destinationParent.Fd()), name)
}
