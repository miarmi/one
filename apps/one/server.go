package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"
)

// Trial 4: is there enough disk on the lab server? This reads the disk that
// holds the app's data folder and the folder's own size. Memory is not shown
// to the owner; each import run logs what the app holds (see importer.go).

type serverInfo struct {
	Free, Total, DataSize string
}

func humanBytes(n uint64) string {
	const gb = 1 << 30
	const mb = 1 << 20
	if n >= gb {
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/mb)
}

func dirSize(dir string) uint64 {
	var total uint64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}

func readServerInfo(dataDir string) (serverInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &st); err != nil {
		return serverInfo{}, err
	}
	bs := uint64(st.Bsize)
	info := serverInfo{
		Free:     humanBytes(uint64(st.Bavail) * bs),
		Total:    humanBytes(uint64(st.Blocks) * bs),
		DataSize: humanBytes(dirSize(dataDir)),
	}
	return info, nil
}

func (s serverInfo) text() string {
	return fmt.Sprintf("Free space: %s of %s\nData folder: %s", s.Free, s.Total, s.DataSize)
}
