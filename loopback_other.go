//go:build !linux

package main

func ensureLoopbackUp() error {
	return nil
}
