//go:build !linux

package server

func freeSpace(string) int64 { return 0 }
