//go:build !windows

package main

import "os"

func openTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDONLY, 0)
}
