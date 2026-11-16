package core

import "strconv"

// MaxOpenDrafts is how many open account preparations one user may hold at
// a time (W1-F13). Callers with the full scope of the route are not limited.
const MaxOpenDrafts = 3

// OpenDraftsLock is the advisory lock (store.XactLock) that serializes,
// until the transaction ends, the count-and-insert of userID's open drafts:
// without it, concurrent requests each count the committed rows only and
// all pass the limit together (READ COMMITTED). Take it right before the
// counting insert.
func OpenDraftsLock(userID int64) string {
	return "open-drafts:" + strconv.FormatInt(userID, 10)
}
