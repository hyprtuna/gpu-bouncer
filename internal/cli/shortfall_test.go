package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
	"github.com/hyprtuna/gpu-bouncer/internal/scheduler"
)

func freeMiB(v uint64) *uint64 { return &v }

func notMet() *bool { met := false; return &met }

// requestReply is a daemon reply to a request that asked for 9000 MiB on a
// card with 7832 MiB free, the shape the shortfall line is written against.
func requestReply(executed []ipc.ActionResult, freeAfter *uint64) ipc.Response {
	return ipc.Response{
		OK: true,
		Plan: &scheduler.Plan{
			Trigger:        scheduler.TriggerRequest,
			Beneficiary:    "up1",
			CurrentFreeMiB: 7832,
			TargetFreeMiB:  9000,
			TotalMiB:       8192,
		},
		Executed:     executed,
		FreeAfterMiB: freeAfter,
		TargetMet:    notMet(),
	}
}

// A plan with nothing to run is not a GPU that could not be read. The
// reading the plan was built on is the measurement, and nothing happened
// after it, so the answer is that 0 MiB were freed.
func TestShortfallOfAnEmptyPlanUsesThePlansOwnReading(t *testing.T) {
	resp := requestReply(nil, nil)

	if got, want := shortfallLine(resp), "freed 0 MiB of the 1168 MiB asked for, target not met"; got != want {
		t.Errorf("shortfall line = %q, want %q", got, want)
	}
	after := planFreeAfter(resp)
	if after == nil || *after != 7832 {
		t.Errorf("free_after_mib = %v, want the plan's own 7832", mibOrUnknown(after))
	}
}

// With actions the reading taken once they had all finished is what counts,
// and it is measured against the first action's before figure.
func TestShortfallOfAPlanWithActionsUsesTheReadingAfterThem(t *testing.T) {
	executed := []ipc.ActionResult{{
		Service: "low", Verb: "release", Acted: true,
		FreeBeforeMiB: freeMiB(7832), FreeAfterMiB: freeMiB(8400),
	}}
	resp := requestReply(executed, freeMiB(8600))

	if got, want := shortfallLine(resp), "freed 768 MiB of the 1168 MiB asked for, target not met"; got != want {
		t.Errorf("shortfall line = %q, want %q", got, want)
	}
	after := planFreeAfter(resp)
	if after == nil || *after != 8600 {
		t.Errorf("free_after_mib = %v, want the daemon's 8600", mibOrUnknown(after))
	}

	// A daemon that sent no overall reading leaves the last action's own.
	resp = requestReply(executed, nil)
	if got, want := shortfallLine(resp), "freed 568 MiB of the 1168 MiB asked for, target not met"; got != want {
		t.Errorf("without an overall reading, shortfall line = %q, want %q", got, want)
	}
}

// An action ran and the reading after it failed. How much was freed is then
// genuinely unknown, and saying so is the one case that may blame the GPU.
func TestShortfallIsUnknownWhenThePostActionReadFailed(t *testing.T) {
	resp := requestReply([]ipc.ActionResult{{
		Service: "low", Verb: "release", Acted: true,
		FreeBeforeMiB: freeMiB(7832), FreeAfterMiB: nil,
		Error: "the GPU could not be read after the action",
	}}, nil)

	want := "how much of the 1168 MiB asked for was freed is not known: the GPU could not be read, target not met"
	if got := shortfallLine(resp); got != want {
		t.Errorf("shortfall line = %q, want %q", got, want)
	}
	if after := planFreeAfter(resp); after != nil {
		t.Errorf("free_after_mib = %d, want null: no reading succeeded", *after)
	}
}

// A plan built without a reading of the card carries a zero total, and no
// figure of its own may be published as free_after_mib.
func TestFreeAfterIsNullWhenThePlanSawNoCard(t *testing.T) {
	resp := ipc.Response{OK: true, Plan: &scheduler.Plan{
		Notes: []string{"the GPU could not be read"},
	}}
	if after := planFreeAfter(resp); after != nil {
		t.Errorf("free_after_mib = %d, want null on an unreadable card", *after)
	}
}

// End to end: the only service is the one asking, so there is nothing to
// evict and the plan is empty. The client must report against the reading
// the daemon took, not report the card as unreadable.
func TestRequestWithNothingToEvictReportsTheShortfall(t *testing.T) {
	startDaemon(t,
		config.Service{Name: "solo", Adapter: config.AdapterOllama, Endpoint: fakeOllama(t, http.StatusOK), Priority: 50},
	)
	code, stdout, _ := run("request", "solo", "--need-mib", "8192")
	if code != 0 {
		t.Fatalf("exit %d, want 0: a shortfall is not a failure", code)
	}
	if got, want := lastLine(stdout), "freed 0 MiB of the 5000 MiB asked for, target not met"; got != want {
		t.Errorf("last line = %q, want %q", got, want)
	}

	_, stdout, _ = run("--json", "request", "solo", "--need-mib", "8192")
	var decoded struct {
		FreeAfterMiB *uint64            `json:"free_after_mib"`
		TargetMet    *bool              `json:"target_met"`
		Executed     []ipc.ActionResult `json:"executed"`
		Plan         scheduler.Plan     `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if len(decoded.Executed) != 0 {
		t.Fatalf("executed = %v, want empty: there is nothing to evict", decoded.Executed)
	}
	if decoded.FreeAfterMiB == nil || *decoded.FreeAfterMiB != decoded.Plan.CurrentFreeMiB {
		t.Errorf("free_after_mib = %v, want the plan's %d", mibOrUnknown(decoded.FreeAfterMiB), decoded.Plan.CurrentFreeMiB)
	}
	if decoded.TargetMet == nil || *decoded.TargetMet {
		t.Errorf("target_met = %v, want false", decoded.TargetMet)
	}
}
