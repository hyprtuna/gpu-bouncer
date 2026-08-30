package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/hyprtuna/gpu-bouncer/internal/config"
	"github.com/hyprtuna/gpu-bouncer/internal/ipc"
)

// The bound a client is told to wait for is timeout plus drain_timeout. A
// config file can no longer set a timeout large enough to overflow that sum,
// but a Config built in code can, and a bound that wrapped negative was read
// as no bound at all: the daemon announced nothing and the client fell back
// to its fixed wait, which is what a long timeout is set to avoid.
func TestAnActionBoundSaturatesRatherThanWrapping(t *testing.T) {
	ordinary := config.Service{
		Timeout:      config.Duration(5 * time.Second),
		DrainTimeout: config.Duration(30 * time.Second),
	}
	if got, want := actionBound(ordinary), 35*time.Second; got != want {
		t.Errorf("actionBound = %s, want %s", got, want)
	}

	// At the config's own ceiling the sum is still exact.
	atMax := config.Service{
		Timeout:      config.Duration(config.MaxServiceTimeout),
		DrainTimeout: config.Duration(config.MaxDrainTimeout),
	}
	if got, want := actionBound(atMax), config.MaxServiceTimeout+config.MaxDrainTimeout; got != want {
		t.Errorf("actionBound at the config maximum = %s, want %s", got, want)
	}

	for _, svc := range []config.Service{
		{Timeout: config.Duration(math.MaxInt64), DrainTimeout: config.Duration(time.Minute)},
		{Timeout: config.Duration(time.Minute), DrainTimeout: config.Duration(math.MaxInt64)},
		{Timeout: config.Duration(math.MaxInt64), DrainTimeout: config.Duration(math.MaxInt64)},
	} {
		bound := actionBound(svc)
		if bound <= 0 {
			t.Errorf("actionBound(timeout %d, drain %d) = %s, want a positive bound",
				svc.Timeout, svc.DrainTimeout, bound)
		}
		// Every deadline downstream is this plus something, and must not
		// wrap either.
		if wait := ipc.WaitFor(bound); wait <= 0 {
			t.Errorf("the client wait for %s is %s, want a positive wait", bound, wait)
		}
		if budget := ipc.WaitFor(bound) + time.Minute; budget <= 0 {
			t.Errorf("the connection budget for %s is %s, want a positive budget", bound, budget)
		}
	}
}
