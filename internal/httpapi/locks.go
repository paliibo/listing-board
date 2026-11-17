package httpapi

import (
	"hash/fnv"
	"sync"
)

// accountLocks lets one request at a time change the sheets of an account.
//
// Attaching a brochure and starting a sheet both read a sheet, decide, and
// write it back, and "one running sheet per account" spans several sheets.
// Holding the account's lock around that sequence keeps two requests from
// deciding on the same stale read, whatever store is behind the API.
//
// The mutex is picked by a hash of the account id from a fixed set, so there
// is nothing to clean up; two accounts that share one only wait for each other.
type accountLocks [64]sync.Mutex

// lock blocks until the account's mutex is free and returns its unlock.
func (l *accountLocks) lock(userID string) (unlock func()) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userID))
	m := &l[h.Sum32()%uint32(len(l))]
	m.Lock()
	return m.Unlock
}
