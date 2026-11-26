package webhook

import "time"

// KeysMaxAge is how long fetched keys are used before they are fetched again.
const KeysMaxAge = keysMaxAge

// SetClock lets a test say what time the receiver takes it to be.
func SetClock(r *Receiver, now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = now
}
