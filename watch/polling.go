// Copyright (c) 2019 FOSS contributors of https://github.com/nxadm/tail
// Copyright (c) 2015 HPE Software Inc. All rights reserved.
// Copyright (c) 2013 ActiveState Software Inc. All rights reserved.

package watch

import (
	"os"
	"runtime"
	"time"

	"gopkg.in/tomb.v1"
)

// PollingFileWatcher polls the file for changes.
type PollingFileWatcher struct {
	Filename string
	Size     int64
}

func NewPollingFileWatcher(filename string) *PollingFileWatcher {
	fw := &PollingFileWatcher{filename, 0}
	return fw
}

var POLL_DURATION time.Duration

func (fw *PollingFileWatcher) BlockUntilExists(t *tomb.Tomb) error {
	for {
		if _, err := os.Stat(fw.Filename); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-time.After(POLL_DURATION):
			continue
		case <-t.Dying():
			return tomb.ErrDying
		}
	}
	panic("unreachable")
}

func (fw *PollingFileWatcher) ChangeEvents(t *tomb.Tomb, pos int64) (*FileChanges, error) {
	origFi, err := os.Stat(fw.Filename)
	if err != nil {
		return nil, err
	}
	return fw.changeEvents(t, origFi, pos), nil
}

// ChangeEventsFile reports changes at Filename relative to the open file f.
// A missing path or a different file is reported as a deletion.
func (fw *PollingFileWatcher) ChangeEventsFile(t *tomb.Tomb, f *os.File, pos int64) (*FileChanges, error) {
	origFi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return fw.changeEvents(t, origFi, pos), nil
}

func (fw *PollingFileWatcher) changeEvents(t *tomb.Tomb, origFi os.FileInfo, pos int64) *FileChanges {
	changes := NewFileChanges()
	var prevModTime time.Time

	fw.Size = pos

	go func() {
		prevSize := fw.Size
		for {
			select {
			case <-t.Dying():
				return
			default:
			}

			time.Sleep(POLL_DURATION)
			fi, err := os.Stat(fw.Filename)
			if err != nil {
				// Windows cannot delete a file if a handle is still open (tail keeps one open)
				// so it gives access denied to anything trying to read it until all handles are released.
				if os.IsNotExist(err) || (runtime.GOOS == "windows" && os.IsPermission(err)) {
					// File does not exist (has been deleted).
					changes.NotifyDeleted()
					return
				}

				// Stop the tail and report the error from Wait, unless
				// it is already stopping.
				select {
				case <-t.Dying():
				default:
					t.Kill(err)
				}
				return
			}

			// File got moved/renamed?
			if !os.SameFile(origFi, fi) {
				changes.NotifyDeleted()
				return
			}

			// File got truncated?
			fw.Size = fi.Size()
			if prevSize > 0 && prevSize > fw.Size {
				changes.NotifyTruncated()
				prevSize = fw.Size
				continue
			}
			// File got bigger?
			if prevSize > 0 && prevSize < fw.Size {
				changes.NotifyModified()
				prevSize = fw.Size
				continue
			}
			prevSize = fw.Size

			// File was appended to (changed)?
			modTime := fi.ModTime()
			if modTime != prevModTime {
				prevModTime = modTime
				changes.NotifyModified()
			}
		}
	}()

	return changes
}

func init() {
	POLL_DURATION = 250 * time.Millisecond
}
