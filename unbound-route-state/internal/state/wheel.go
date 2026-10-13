package state

import "time"

// wheel is a single-level timing wheel: granularity × len(slots)
// covers the "horizon"; entries whose expiry falls further out than that
// live in a separate overflow list and are migrated into a slot once the
// wheel's advancing "now" brings them within range.
//
// Membership in a slot or in overflow is an intrusive doubly linked list
// threaded directly through ipState.wheelNext/wheelPrev - no separate
// list nodes are allocated, so scheduling/rescheduling an entry costs no
// extra heap allocation beyond the ipState itself.
type wheel struct {
	granularitySeconds uint32
	slots              []*ipState // one doubly linked list head per slot
	current            int        // index of the slot representing "now"
	nowSlotTime        uint32     // unix seconds boundary of the `current` slot
	overflow           *ipState   // entries beyond the horizon
}

func newWheel(granularity time.Duration, numSlots int, startUnix uint32) *wheel {
	g := uint32(granularity / time.Second)
	if g == 0 {
		g = 1
	}
	if numSlots <= 0 {
		numSlots = 3600
	}
	return &wheel{
		granularitySeconds: g,
		slots:              make([]*ipState, numSlots),
		nowSlotTime:        startUnix,
	}
}

func (w *wheel) horizonSeconds() uint32 {
	return w.granularitySeconds * uint32(len(w.slots))
}

// insert schedules e to be picked up by a future tick() once expiresAt is
// reached, based on the *current* maxExpiry(e) the caller has already
// written into e.domains. e must not already be linked into the wheel —
// callers renewing an existing entry must call remove() first.
func (w *wheel) insert(e *ipState, expiresAt uint32) {
	e.slot = -1
	e.inOverflow = false

	if expiresAt <= w.nowSlotTime {
		// Already due (e.g. TTL=0, or a renewal that only extended one of
		// several domains while the others already lapsed). tick()
		// advances current *before* processing, so the slot visited on the
		// very next tick() call is current+1, not current itself. Linking
		// into current would strand the entry in an already-passed slot for a
		// full revolution of the wheel. The delay is at most one granularity
		// period, consistent with the documented +/- granularity precision.
		idx := (w.current + 1) % len(w.slots)
		w.linkInto(&w.slots[idx], e)
		e.slot = idx
		return
	}

	delta := expiresAt - w.nowSlotTime
	if delta >= w.horizonSeconds() {
		w.linkInto(&w.overflow, e)
		e.inOverflow = true
		return
	}

	ticksAhead := (delta + w.granularitySeconds - 1) / w.granularitySeconds // ceil div
	idx := (w.current + int(ticksAhead)) % len(w.slots)
	w.linkInto(&w.slots[idx], e)
	e.slot = idx
}

// remove unlinks e from whichever slot or overflow list currently holds
// it. Safe to call on an entry that isn't linked anywhere (slot == -1 and
// inOverflow == false) - a no-op in that case, which keeps callers in
// store.go simple (they don't need to track whether an insert already
// happened).
func (w *wheel) remove(e *ipState) {
	if e.inOverflow {
		w.unlinkFrom(&w.overflow, e)
		e.inOverflow = false
		return
	}
	if e.slot >= 0 {
		w.unlinkFrom(&w.slots[e.slot], e)
		e.slot = -1
	}
}

func (w *wheel) linkInto(head **ipState, e *ipState) {
	e.wheelNext = *head
	e.wheelPrev = nil
	if *head != nil {
		(*head).wheelPrev = e
	}
	*head = e
}

func (w *wheel) unlinkFrom(head **ipState, e *ipState) {
	if e.wheelPrev != nil {
		e.wheelPrev.wheelNext = e.wheelNext
	} else {
		*head = e.wheelNext
	}
	if e.wheelNext != nil {
		e.wheelNext.wheelPrev = e.wheelPrev
	}
	e.wheelNext = nil
	e.wheelPrev = nil
}

// tick processes whatever is due "now" (the slot the wheel currently
// points at), then advances current/nowSlotTime by one granularity step
// and migrates any overflow entries that now fall within the horizon.
//
// Returned entries are already unlinked from the wheel; the caller
// (Store.handleTick) decides whether to withdraw them or reschedule them
// (a domain may have renewed after this entry was originally scheduled).
func (w *wheel) tick() []*ipState {
	w.current = (w.current + 1) % len(w.slots)
	w.nowSlotTime += w.granularitySeconds

	var due []*ipState
	for e := w.slots[w.current]; e != nil; {
		next := e.wheelNext
		e.wheelNext, e.wheelPrev = nil, nil
		e.slot = -1
		due = append(due, e)
		e = next
	}
	w.slots[w.current] = nil

	w.migrateOverflow()
	return due
}

// migrateOverflow moves entries from the overflow list into a real slot
// once their expiry has come within the (now advanced) horizon.
func (w *wheel) migrateOverflow() {
	e := w.overflow
	for e != nil {
		next := e.wheelNext
		exp := maxExpiry(e)
		if exp-w.nowSlotTime < w.horizonSeconds() {
			w.unlinkFrom(&w.overflow, e)
			w.insert(e, exp)
		}
		e = next
	}
}

// fastForward snaps the wheel's internal clock directly to t without
// visiting slots one by one. Only safe to call once a full revolution of
// ordinary tick() calls has already happened in the same catch-up pass
// (Store.handleTick guarantees this) - at that point every slot is
// already known to be empty, so nothing is lost by skipping straight to
// t. migrateOverflow still runs so overflow entries reschedule correctly
// against the new horizon.
func (w *wheel) fastForward(t uint32) {
	w.nowSlotTime = t
	w.migrateOverflow()
}

// maxDomainExpiry is the latest DNS-driven expiry across all domains still
// contributing to e (0 if none is left): an IP lives as long as its
// longest-lived domain.
func maxDomainExpiry(e *ipState) uint32 {
	var m uint32
	for _, d := range e.domains {
		if d.expiresAt > m {
			m = d.expiresAt
		}
	}
	return m
}

// maxExpiry is when the route is due to be withdrawn: the latest DNS-driven
// expiry, or the traffic hold if that is later.
func maxExpiry(e *ipState) uint32 {
	m := maxDomainExpiry(e)
	if e.holdUntil > m {
		m = e.holdUntil
	}
	return m
}
