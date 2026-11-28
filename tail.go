// Copyright (c) 2019 FOSS contributors of https://github.com/nxadm/tail
// Copyright (c) 2015 HPE Software Inc. All rights reserved.
// Copyright (c) 2013 ActiveState Software Inc. All rights reserved.

//nxadm/tail provides a Go library that emulates the features of the BSD `tail`
//program. The library comes with full support for truncation/move detection as
//it is designed to work with log rotation tools. The library works on all
//operating systems supported by Go, including POSIX systems like Linux and
//*BSD, and MS Windows. Go 1.18 is the oldest compiler release supported.
package tail

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nxadm/tail/ratelimiter"
	"github.com/nxadm/tail/util"
	"github.com/nxadm/tail/watch"
	"gopkg.in/tomb.v1"
)

var (
	// ErrStop is returned when the tail of a file has been marked to be stopped.
	ErrStop = errors.New("tail should now stop")
)

type Line struct {
	Text     string    // The contents of the file
	Num      int       // The line number
	SeekInfo SeekInfo  // SeekInfo
	Time     time.Time // Present time
	Err      error     // Error from tail
}

// Deprecated: this function is no longer used internally and it has little of no
// use in the API. As such, it will be removed from the API in a future major
// release.
//
// NewLine returns a * pointer to a Line struct.
func NewLine(text string, lineNum int) *Line {
	return &Line{text, lineNum, SeekInfo{}, time.Now(), nil}
}

// SeekInfo represents arguments to io.Seek. See: https://golang.org/pkg/io/#SectionReader.Seek
type SeekInfo struct {
	Offset int64
	Whence int
}

type logger interface {
	Fatal(v ...interface{})
	Fatalf(format string, v ...interface{})
	Fatalln(v ...interface{})
	Panic(v ...interface{})
	Panicf(format string, v ...interface{})
	Panicln(v ...interface{})
	Print(v ...interface{})
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// Config is used to specify how a file must be tailed.
type Config struct {
	// File-specifc
	Location  *SeekInfo // Tail from this location. If nil, start at the beginning of the file
	ReOpen    bool      // Reopen recreated files (tail -F)
	MustExist bool      // Fail early if the file does not exist
	Poll      bool      // Poll for file changes instead of using the default inotify
	Pipe      bool      // The file is a named pipe (mkfifo)

	// Generic IO
	Follow        bool // Continue looking for new lines (tail -f)
	MaxLineSize   int  // If non-zero, split longer lines into multiple lines
	CompleteLines bool // Only return complete lines (that end with "\n" or EOF when Follow is false)

	// Optionally, use a ratelimiter (e.g. created by the ratelimiter/NewLeakyBucket function)
	RateLimiter *ratelimiter.LeakyBucket

	// Optionally use a Logger. When nil, the Logger is set to tail.DefaultLogger.
	// To disable logging, set it to tail.DiscardingLogger
	Logger logger
}

type Tail struct {
	Filename string     // The filename
	Lines    chan *Line // A consumable channel of *Line
	Config              // Tail.Configuration

	file    *os.File
	reader  *bufio.Reader
	lineNum int

	lineBuf *strings.Builder

	watcher watch.FileWatcher
	changes *watch.FileChanges

	reopenPending bool     // Switch files after draining the current reader.
	replacement   *os.File // Opened when the move was detected, if it existed.

	tomb.Tomb // provides: Done, Kill, Dying

	lk sync.Mutex
}

var (
	// DefaultLogger logs to os.Stderr and it is used when Config.Logger == nil
	DefaultLogger = log.New(os.Stderr, "", log.LstdFlags)
	// DiscardingLogger can be used to disable logging output
	DiscardingLogger = log.New(ioutil.Discard, "", 0)
)

// TailFile begins tailing the file. And returns a pointer to a Tail struct
// and an error. An output stream is made available via the Tail.Lines
// channel (e.g. to be looped and printed). To handle errors during tailing,
// after finishing reading from the Lines channel, invoke the `Wait` or `Err`
// method on the returned *Tail.
func TailFile(filename string, config Config) (*Tail, error) {
	if config.ReOpen && !config.Follow {
		util.Fatal("cannot set ReOpen without Follow.")
	}

	t := &Tail{
		Filename: filename,
		Lines:    make(chan *Line),
		Config:   config,
	}

	if config.CompleteLines {
		t.lineBuf = new(strings.Builder)
	}

	// when Logger was not specified in config, use default logger
	if t.Logger == nil {
		t.Logger = DefaultLogger
	}

	if t.Poll {
		t.watcher = watch.NewPollingFileWatcher(filename)
	} else {
		t.watcher = watch.NewInotifyFileWatcher(filename)
	}

	if t.MustExist {
		var err error
		t.file, err = OpenFile(t.Filename)
		if err != nil {
			return nil, err
		}
	}

	go t.tailFileSync()

	return t, nil
}

// Tell returns the file's current position, like stdio's ftell() and an error.
// Beware that this value may not be completely accurate because one line from
// the chan(tail.Lines) may have been read already.
func (tail *Tail) Tell() (offset int64, err error) {
	if tail.file == nil {
		return
	}
	offset, err = tail.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	tail.lk.Lock()
	defer tail.lk.Unlock()
	if tail.reader == nil {
		return
	}

	offset -= int64(tail.reader.Buffered())
	return
}

// Stop stops the tailing activity.
func (tail *Tail) Stop() error {
	tail.Kill(nil)
	return tail.Wait()
}

// StopAtEOF stops tailing as soon as the end of the file is reached. The function
// returns an error,
func (tail *Tail) StopAtEOF() error {
	tail.Kill(errStopAtEOF)
	return tail.Wait()
}

var errStopAtEOF = errors.New("tail: stop at eof")

func (tail *Tail) close() {
	close(tail.Lines)
	tail.closeFile()
	if tail.replacement != nil {
		tail.replacement.Close()
		tail.replacement = nil
	}
}

func (tail *Tail) closeFile() {
	if tail.file != nil {
		tail.file.Close()
		tail.file = nil
	}
}

func (tail *Tail) reopen() error {
	if tail.lineBuf != nil {
		tail.lineBuf.Reset()
	}
	tail.closeFile()
	tail.lineNum = 0
	if tail.replacement != nil {
		tail.file, tail.replacement = tail.replacement, nil
		return nil
	}
	for {
		var err error
		tail.file, err = OpenFile(tail.Filename)
		if err != nil {
			if os.IsNotExist(err) {
				tail.Logger.Printf("Waiting for %s to appear...", tail.Filename)
				if err := tail.watcher.BlockUntilExists(&tail.Tomb); err != nil {
					if err == tomb.ErrDying {
						return err
					}
					return fmt.Errorf("Failed to detect creation of %s: %s", tail.Filename, err)
				}
				continue
			}
			return fmt.Errorf("Unable to open file %s: %s", tail.Filename, err)
		}
		break
	}
	return nil
}

func (tail *Tail) readLine() (string, error) {
	tail.lk.Lock()
	line, err := tail.reader.ReadString('\n')
	tail.lk.Unlock()

	newlineEnding := strings.HasSuffix(line, "\n")
	line = strings.TrimRight(line, "\n")

	// if we don't have to handle incomplete lines, we can return the line as-is
	if !tail.Config.CompleteLines {
		// Note ReadString "returns the data read before the error" in
		// case of an error, including EOF, so we return it as is. The
		// caller is expected to process it if err is EOF.
		return line, err
	}

	if _, err := tail.lineBuf.WriteString(line); err != nil {
		return line, err
	}

	if newlineEnding {
		line = tail.lineBuf.String()
		tail.lineBuf.Reset()
		return line, nil
	} else {
		if tail.Config.Follow {
			line = ""
		}
		return line, io.EOF
	}
}

func (tail *Tail) tailFileSync() {
	defer tail.Done()
	defer tail.close()

	if !tail.MustExist {
		// deferred first open.
		err := tail.reopen()
		if err != nil {
			if err != tomb.ErrDying {
				tail.Kill(err)
			}
			return
		}
	}

	// Seek to requested location on first open of the file.
	if tail.Location != nil {
		_, err := tail.file.Seek(tail.Location.Offset, tail.Location.Whence)
		if err != nil {
			tail.Killf("Seek error on %s: %s", tail.Filename, err)
			return
		}
	}

	tail.openReader()

	// Read line by line.
	for {
		// do not seek in named pipes
		if !tail.Pipe {
			// grab the position in case we need to back up in the event of a half-line
			if _, err := tail.Tell(); err != nil {
				tail.Kill(err)
				return
			}
		}

		line, err := tail.readLine()

		// Process `line` even if err is EOF.
		if err == nil {
			cooloff := !tail.sendLine(line)
			if cooloff {
				// Wait a second before seeking till the end of
				// file when rate limit is reached.
				msg := ("Too much log activity; waiting a second before resuming tailing")
				offset, _ := tail.Tell()
				select {
				case tail.Lines <- &Line{msg, tail.lineNum, SeekInfo{Offset: offset}, time.Now(), errors.New(msg)}:
				case <-tail.Dying():
					return
				}
				select {
				case <-time.After(time.Second):
				case <-tail.Dying():
					return
				}
				// A pending reopen lets us skip the remaining old lines.
				if tail.reopenPending {
					err = tail.reopenAfterMove()
				} else {
					err = tail.seekEnd()
				}
				if err != nil {
					tail.Kill(err)
					return
				}
			}
		} else if err == io.EOF {
			if !tail.Follow {
				if line != "" {
					tail.sendLine(line)
				}
				return
			}

			if tail.Follow && line != "" {
				tail.sendLine(line)
				if err := tail.seekEnd(); err != nil {
					tail.Kill(err)
					return
				}
			}

			// When EOF is reached, wait for more data to become
			// available. Wait strategy is based on the `tail.watcher`
			// implementation (inotify or polling).
			err := tail.waitForChanges()
			if err != nil {
				if err != ErrStop {
					tail.Kill(err)
				}
				return
			}
		} else {
			// non-EOF error
			tail.Killf("Error reading %s: %s", tail.Filename, err)
			return
		}

		select {
		case <-tail.Dying():
			if tail.Err() == errStopAtEOF {
				continue
			}
			return
		default:
		}
	}
}

// openFileWatcher compares the watched path with the open file's identity.
type openFileWatcher interface {
	ChangeEventsFile(*tomb.Tomb, *os.File, int64) (*watch.FileChanges, error)
}

// waitForChanges waits at EOF for a file change or completes a pending reopen.
func (tail *Tail) waitForChanges() error {
	if tail.reopenPending {
		select {
		case <-tail.Dying():
			// Stop, or StopAtEOF at the end of the old file.
			return ErrStop
		default:
		}
		return tail.reopenAfterMove()
	}
	if tail.changes == nil {
		pos, err := tail.file.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if w, ok := tail.watcher.(openFileWatcher); ok {
			tail.changes, err = w.ChangeEventsFile(&tail.Tomb, tail.file, pos)
		} else {
			tail.changes, err = tail.watcher.ChangeEvents(&tail.Tomb, pos)
		}
		if err != nil {
			return err
		}
	}

	select {
	case <-tail.changes.Modified:
		return nil
	case <-tail.changes.Deleted:
		tail.changes = nil
		if _, ok := tail.watcher.(openFileWatcher); ok && tail.ReOpen {
			// Bound the drain by the old file's current size.
			// Open the replacement now to keep it if it moves during
			// the drain. The watcher detects that move by comparing
			// the path with the open replacement.
			if fi, err := tail.file.Stat(); err == nil && fi.Mode().IsRegular() {
				if pos, err := tail.file.Seek(0, io.SeekCurrent); err == nil {
					tail.lk.Lock()
					tail.reader.Reset(io.LimitReader(tail.file, fi.Size()-pos))
					tail.lk.Unlock()
				}
			}
			if f, err := OpenFile(tail.Filename); err == nil {
				tail.replacement = f
			}
			tail.reopenPending = true
			return nil
		}
		if tail.ReOpen {
			return tail.reopenAfterMove()
		}
		tail.Logger.Printf("Stopping tail as file no longer exists: %s", tail.Filename)
		return ErrStop
	case <-tail.changes.Truncated:
		// Always reopen truncated files (Follow is true)
		tail.Logger.Printf("Re-opening truncated file %s ...", tail.Filename)
		if err := tail.reopen(); err != nil {
			return err
		}
		tail.Logger.Printf("Successfully reopened truncated %s", tail.Filename)
		tail.openReader()
		return nil
	case <-tail.Dying():
		return ErrStop
	}
}

// reopenAfterMove switches to the replacement, waiting for it if needed.
func (tail *Tail) reopenAfterMove() error {
	tail.reopenPending = false
	// XXX: we must not log from a library.
	tail.Logger.Printf("Re-opening moved/deleted file %s ...", tail.Filename)
	if err := tail.reopen(); err != nil {
		return err
	}
	tail.Logger.Printf("Successfully reopened %s", tail.Filename)
	tail.openReader()
	return nil
}

func (tail *Tail) openReader() {
	tail.lk.Lock()
	if tail.MaxLineSize > 0 {
		// add 2 to account for newline characters
		tail.reader = bufio.NewReaderSize(tail.file, tail.MaxLineSize+2)
	} else {
		tail.reader = bufio.NewReader(tail.file)
	}
	tail.lk.Unlock()
}

func (tail *Tail) seekEnd() error {
	return tail.seekTo(SeekInfo{Offset: 0, Whence: io.SeekEnd})
}

func (tail *Tail) seekTo(pos SeekInfo) error {
	_, err := tail.file.Seek(pos.Offset, pos.Whence)
	if err != nil {
		return fmt.Errorf("Seek error on %s: %s", tail.Filename, err)
	}
	// Reset the read buffer whenever the file is re-seek'ed
	tail.reader.Reset(tail.file)
	return nil
}

// sendLine sends the line(s) to Lines channel, splitting longer lines
// if necessary. Return false if rate limit is reached.
func (tail *Tail) sendLine(line string) bool {
	now := time.Now()
	lines := []string{line}

	// Split longer lines
	if tail.MaxLineSize > 0 && len(line) > tail.MaxLineSize {
		lines = util.PartitionString(line, tail.MaxLineSize)
	}

	for _, line := range lines {
		tail.lineNum++
		offset, _ := tail.Tell()
		select {
		case tail.Lines <- &Line{line, tail.lineNum, SeekInfo{Offset: offset}, now, nil}:
		case <-tail.Dying():
			return true
		}
	}

	if tail.Config.RateLimiter != nil {
		ok := tail.Config.RateLimiter.Pour(uint16(len(lines)))
		if !ok {
			tail.Logger.Printf("Leaky bucket full (%v); entering 1s cooloff period.",
				tail.Filename)
			return false
		}
	}

	return true
}

// Cleanup removes inotify watches added by the tail package. This function is
// meant to be invoked from a process's exit handler. Linux kernel may not
// automatically remove inotify watches after the process exits.
// If you plan to re-read a file, don't call Cleanup in between.
func (tail *Tail) Cleanup() {
	watch.Cleanup(tail.Filename)
}
