package store

import "errors"

// SQLite extended result codes retain their primary code in the low byte.
// Do not classify arbitrary error text or corruption as transient contention.
func IsContention(err error) bool {
	if errors.Is(err, ErrWriterQueueFull) {
		return true
	}
	var coded interface{ Code() int }
	return errors.As(err, &coded) && (coded.Code()&255 == 5 || coded.Code()&255 == 6)
}
