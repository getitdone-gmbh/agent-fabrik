package server

import "time"

// SetTimeouts shortens the slow-client deadlines for a test.
func SetTimeouts(read, write time.Duration) (restore func()) {
	r, w := bodyReadTimeout, writeTimeout
	bodyReadTimeout, writeTimeout = read, write
	return func() { bodyReadTimeout, writeTimeout = r, w }
}
