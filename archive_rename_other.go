//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func renamePinnedArchive(sourceParent, destinationParent *os.File, name string) error {
	return errors.New("safe ignored-file archival is supported only on macOS and Linux; original files retained")
}
