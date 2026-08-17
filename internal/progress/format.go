package progress

import (
	"fmt"
	"io"
)

// NewCountingReader wraps r, invoking onRead with the cumulative byte count
// after every successful read — the data source for the upload counter (C1).
func NewCountingReader(r io.Reader, onRead func(total int64)) io.Reader {
	return &countingReader{r: r, onRead: onRead}
}

type countingReader struct {
	r      io.Reader
	n      int64
	onRead func(total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.onRead != nil {
			c.onRead(c.n)
		}
	}
	return n, err
}

// FormatBytes renders a byte count for the live counter (C1): "42.3 MB",
// "812 KB", "1.2 GB".
func FormatBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%d KB", n/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FormatCount renders an integer with thousands separators: "1,204".
func FormatCount(n int64) string {
	if n < 0 {
		return "-" + FormatCount(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return FormatCount(n/1000) + fmt.Sprintf(",%03d", n%1000)
}
