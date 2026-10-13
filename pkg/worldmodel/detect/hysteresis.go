package detect

// Side is where a Hysteresis last saw its value.
type Side int8

// The sides. SideUnknown is the state before the value has left the band once.
const (
	SideBelow   Side = -1
	SideUnknown Side = 0
	SideAbove   Side = 1
)

// Hysteresis tracks which side of a band a value is on, so that a crossing
// trigger fires once when the value goes through the band and not repeatedly while
// it jitters around a single threshold. A value at or above Upper is Above, at or
// below Lower is Below, and in between the side does not change. Lower must not
// exceed Upper.
//
// The first time the value leaves the band it only establishes the side: that is
// not a crossing, because nothing was known to cross from.
type Hysteresis struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
	State Side    `json:"state"`
}

// Update folds x in and reports the side and whether this value crossed from the
// opposite side. A non-finite x changes nothing.
func (h *Hysteresis) Update(x float64) (side Side, crossed bool) {
	if !finite(x) {
		return h.State, false
	}
	prev := h.State
	switch {
	case x >= h.Upper:
		h.State = SideAbove
	case x <= h.Lower:
		h.State = SideBelow
	}
	return h.State, prev != SideUnknown && h.State != prev
}
