package line

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

const (
	MaxLineLength       = 128
	TicksPerMs    int64 = 10_000
	TicksPerSec         = 1_000 * TicksPerMs
)

// FormatLine creates a line with the same format as the C# implementation
func FormatLine(row int64, eol bool) []byte {
	var result string

	c := []byte{byte(row%26) + byte('A')}

	// Generate string similar to:
	// abc,{row},def,{repeated char},ghi,{GetTime1(row)},jkl,{GetTime2(row)},mno
	result = fmt.Sprintf("abc,%d,def,%s,ghi,%s,jkl,%s,mno",
		row,
		bytes.Repeat(c, int(row%13)+1),
		getTime1(row).Format(time.RFC3339Nano),
		getTime2(row).Format(time.RFC3339Nano))

	if eol {
		result += "\n"
	}

	return []byte(result)
}

func getTime1(row int64) time.Time {
	return time.UnixMicro(row * 10) // equivalent to row * TicksPerMs, in microseconds
}

func getTime2(row int64) time.Time {
	return time.UnixMilli(row * 10) // equivalent to row * TicksPerSec, in milliseconds
}

// WriteOptions configures how lines are written to the stream
type WriteOptions struct {
	Delay        time.Duration
	Logger       *logr.Logger
	MaxRows      int64
	RowsPerDelay int64
	RowsPerLog   int64
}

// NewLocalWriteOptions creates write options for local streaming
func NewLocalWriteOptions(logger *logr.Logger) *WriteOptions {
	return &WriteOptions{
		Delay:        100 * time.Millisecond,
		Logger:       logger,
		MaxRows:      20_000_000,
		RowsPerDelay: 1_000,
		RowsPerLog:   50_000,
	}
}

// NewWebWriteOptions creates write options for web streaming
func NewWebWriteOptions(logger *logr.Logger) *WriteOptions {
	return &WriteOptions{
		Logger:     logger,
		MaxRows:    20_000_000,
		RowsPerLog: 10_000,
	}
}

// WriteLinesAsync writes lines to the given stream based on the provided options
func WriteLinesAsync(w io.Writer, opts *WriteOptions, done <-chan struct{}) {
	var row int64
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for row < opts.MaxRows {
			select {
			case <-done:
				return
			default:
				line := FormatLine(row, true)

				// Split the write into two parts to simulate the C# implementation
				split := int(row%(int64(len(line)/2))) + 1

				_, err := w.Write(line[:split])
				if err != nil {
					if opts.Logger != nil {
						opts.Logger.Error(err, "error writing first part of line")
					}
					return
				}

				_, err = w.Write(line[split:])
				if err != nil {
					if opts.Logger != nil {
						opts.Logger.Error(err, "error writing second part of line")
					}
					return
				}

				row++

				if opts.RowsPerDelay > 0 && row%opts.RowsPerDelay == 0 {
					select {
					case <-time.After(opts.Delay):
					case <-done:
						return
					}
				}

				if opts.Logger != nil && row%opts.RowsPerLog == 0 {
					opts.Logger.Info("Wrote lines", "count", row)
				}
			}
		}
	}()

	wg.Wait()
}
