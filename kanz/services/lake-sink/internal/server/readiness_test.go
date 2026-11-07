package server

import "testing"

func TestOptionalSnapshotReadinessCannotBeOverwrittenByLanding(t *testing.T) {
	var ordinary Readiness
	ordinary.Set(true)
	if !ordinary.Ready() {
		t.Fatal("default landing readiness changed")
	}
	var enabled Readiness
	enabled.RequireSnapshot()
	enabled.Set(true)
	if enabled.Ready() {
		t.Fatal("landing bypassed snapshot startup")
	}
	enabled.SetSnapshot(true)
	if !enabled.Ready() {
		t.Fatal("both owners ready refused")
	}
	enabled.SetSnapshot(false)
	enabled.Set(true)
	if enabled.Ready() {
		t.Fatal("landing cleared snapshot failure")
	}
	enabled.Set(false)
	enabled.SetSnapshot(true)
	if enabled.Ready() {
		t.Fatal("snapshot bypassed landing failure")
	}
}
