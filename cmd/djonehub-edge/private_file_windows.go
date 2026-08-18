//go:build windows

package main

func readPrivateEdgeFile(string, int64) ([]byte, error) {
	return nil, errEdgeConfiguration
}
