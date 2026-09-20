package metered

import "testing"

// TestShouldApplyStopsRetrying: ifconfig exiting 0 is not proof the flags
// stuck. When a link keeps reading back unmarked, the reconciler must stop
// re-applying rather than spawn a subprocess and log a line every tick — and
// must start over the moment the interface or the wanted state changes.
func TestShouldApplyStopsRetrying(t *testing.T) {
	c := &Controller{}
	for i := 1; i <= applyLimit; i++ {
		if !c.shouldApply("en6", true) {
			t.Fatalf("attempt %d refused, want it allowed (limit %d)", i, applyLimit)
		}
	}
	if c.shouldApply("en6", true) {
		t.Errorf("attempt %d allowed, want it refused", applyLimit+1)
	}

	// a replug under a new name is a fresh situation
	if !c.shouldApply("en7", true) {
		t.Error("a new interface was refused, want a fresh budget")
	}
	// so is the user turning the setting the other way
	if !c.shouldApply("en7", false) {
		t.Error("the opposite change was refused, want a fresh budget")
	}
	// and so is the link finally reading back as asked
	c.attempt = applyAttempt{iface: "en7", want: false, n: applyLimit + 5}
	c.applied()
	if !c.shouldApply("en7", false) {
		t.Error("refused after the link came good, want the counter reset")
	}
}
