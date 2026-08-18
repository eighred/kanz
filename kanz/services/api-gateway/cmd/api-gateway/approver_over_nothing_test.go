package main

import (
	"context"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/config"
)

// AN APPROVER NAMED OVER NO BACKEND USED TO LOG AS A WORKING CONTROL (#539).
//
// # How this was found
//
// Not by reading. I set API_GATEWAY_APPROVE_ROLE in dev/docker-compose.yml to
// make #539's path exercisable somewhere, then checked what that rig actually
// runs: risk-engine and this gateway. Neither dual-control backend is there. The
// setting registered nothing — and the composition root logged
//
//	INFO "api-gateway: override surface fronted" role=kanz-compliance
//
// because the branch keyed on the ROLE alone, while the routes are conditional
// on the role AND the client. The one signal an operator has for whether
// maker-checker is live said yes at a configuration where every override
// endpoint answers 404.
//
// # Why this is the inverse of #535 rather than a duplicate of it
//
// #535 is a capability carried by no role: the route is mounted, nobody holds
// the grant, everyone gets 403. This is a role carried by no route: the grant
// exists, nothing is mounted, everyone gets 404. Both end at "the control is not
// there", and this direction deceives harder — the config names an approver and
// the startup log agreed with it.
//
// # Why a test rather than a comment
//
// The warning lives at the composition root, which no unit test reaches and
// which has shipped crashes past a green suite. buildProxy takes a logger, so
// the posture IS assertable — and it is asserted on the LEVEL, because the
// defect was never a missing line. It was an INFO where a WARN belonged, which
// is the same trap captureHandler's own doc was written for.
func TestAnApproverOverNoBackendIsWarnedAboutNotCongratulated(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			// THE CONFIGURATION I NEARLY SHIPPED TO dev/docker-compose.yml.
			name: "approver, no datamaster",
			cfg:  config.Config{ApproveRole: "kanz-compliance"},
			want: "no API_GATEWAY_DATAMASTER_ADDR",
		},
		{
			// THE HALF THAT LOOKS HARMLESS AND IS NOT. The approve route registers
			// on the role alone, so this deployment CAN sign — it just cannot
			// discover WHAT to sign, and the approve route refuses an empty digest
			// with 400.
			name: "approver and datamaster, no OMS read surface",
			cfg:  config.Config{ApproveRole: "kanz-compliance", DataMasterAddr: "datamaster:9090"},
			want: "no API_GATEWAY_OMS_READ_ADDR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureHandler{}

			// The returned error is expected and irrelevant: an unreachable upstream
			// fails the dial, and the posture logging happens before it.
			_, _ = buildProxy(context.Background(), tc.cfg, cap.logger())

			if cap.hasWarn(tc.want) {
				return
			}
			if cap.hasInfo(tc.want) || cap.hasInfo("override surface fronted") {
				t.Fatalf("the missing backend was reported at INFO, or the surface was reported as "+
					"FRONTED.\n\nThat is the defect itself: a posture that is wrong logged at the "+
					"level a posture that is right uses, so an operator arming maker-checker reads "+
					"confirmation.\n\nlog was:\n%s", cap.all())
			}
			t.Fatalf("nothing logged about %q.\n\nAn operator has no other signal that the routes did "+
				"not register: the config names an approver and the estate answers 404.\n\nlog was:\n%s",
				tc.want, cap.all())
		})
	}
}

// AND A FULLY WIRED SURFACE MUST STILL REPORT AS FRONTED. Without this the guard
// above is satisfied by warning unconditionally, which is how real warnings stop
// being read.
func TestAFrontedOverrideSurfaceIsStillReportedAsFronted(t *testing.T) {
	cap := &captureHandler{}

	_, _ = buildProxy(context.Background(), config.Config{
		ApproveRole:    "kanz-compliance",
		DataMasterAddr: "datamaster:9090",
		OMSReadAddr:    "oms:9090",
	}, cap.logger())

	if !cap.hasInfo("override surface fronted") {
		t.Fatalf("a fully wired override surface reported nothing at INFO:\n%s", cap.all())
	}
	if cap.hasWarn("API_GATEWAY_APPROVE_ROLE") {
		t.Errorf("the wired configuration still warns about a missing backend — the condition is "+
			"inverted or unguarded, and the guard above would then pass by warning always:\n%s", cap.all())
	}
}
