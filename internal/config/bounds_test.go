package config

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// numericFields lists the TOML keys of every numeric field of a struct type:
// integers of any width and Duration. Booleans and strings are not numbers.
func numericFields(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key := f.Tag.Get("toml")
		if key == "" || key == "-" {
			continue
		}
		switch {
		case f.Type == reflect.TypeOf(Duration(0)):
			keys = append(keys, key)
		case f.Type.Kind() >= reflect.Int && f.Type.Kind() <= reflect.Uint64:
			keys = append(keys, key)
		}
	}
	return keys
}

// Invariant: every numeric key of Policy and Service has exactly one bound,
// and every bound names a real key. A key added without a bound fails here,
// which is the point: an unsigned field that silently wraps a negative number
// is how a typed minus sign turned the cooldown off.
func TestEveryNumericKeyHasABound(t *testing.T) {
	for _, tt := range []struct {
		table string
		typ   reflect.Type
	}{
		{"policy", reflect.TypeOf(Policy{})},
		{"service", reflect.TypeOf(Service{})},
	} {
		bounded := map[string]int{}
		for _, b := range NumericBounds {
			if b.Table == tt.table {
				bounded[b.Key]++
			}
		}
		fields := numericFields(t, tt.typ)
		if len(fields) == 0 {
			t.Fatalf("%s: found no numeric fields, the reflection is broken", tt.table)
		}
		for _, key := range fields {
			if bounded[key] != 1 {
				t.Errorf("%s.%s has %d bound(s) in NumericBounds, want exactly 1", tt.table, key, bounded[key])
			}
			delete(bounded, key)
		}
		for key := range bounded {
			t.Errorf("NumericBounds names %s.%s, which is not a numeric field", tt.table, key)
		}
	}
	// The unsigned fields in particular: each must have a non negative Min,
	// so that no value a file can write reaches them through wrapping.
	for _, tt := range []struct {
		table string
		typ   reflect.Type
	}{{"policy", reflect.TypeOf(Policy{})}, {"service", reflect.TypeOf(Service{})}} {
		for i := 0; i < tt.typ.NumField(); i++ {
			f := tt.typ.Field(i)
			if f.Type.Kind() < reflect.Uint || f.Type.Kind() > reflect.Uint64 {
				continue
			}
			for _, b := range NumericBounds {
				if b.Table == tt.table && b.Key == f.Tag.Get("toml") && (b.Signed || b.Min < 0) {
					t.Errorf("%s.%s is unsigned but its bound allows negative values", tt.table, b.Key)
				}
			}
		}
	}
}

// Every key at -1, 0 and its smallest legal value, through a real file.
func TestNumericBoundsPerKey(t *testing.T) {
	render := func(b Bound, v int64) string {
		var value string
		if b.Duration {
			value = fmt.Sprintf("%q", time.Duration(v).String())
		} else {
			value = fmt.Sprintf("%d", v)
		}
		if b.Table == "policy" {
			return fmt.Sprintf("[policy]\n%s = %s\n", b.Key, value)
		}
		return serviceHead + fmt.Sprintf("%s = %s\n", b.Key, value)
	}
	for _, b := range NumericBounds {
		smallest := b.Min
		if b.Signed {
			smallest = -1
		}
		cases := []struct {
			name    string
			value   int64
			wantErr bool
		}{
			{"-1", -1, !b.Signed},
			{"0", 0, !b.Signed && b.Min > 0},
			{"smallest legal", smallest, false},
		}
		for _, tc := range cases {
			if b.Signed && tc.value == math.MinInt64 {
				continue
			}
			t.Run(b.Table+"."+b.Key+"/"+tc.name, func(t *testing.T) {
				path := writeFile(t, t.TempDir(), "c.toml", render(b, tc.value))
				cfg, err := LoadFrom([]string{path})
				if tc.wantErr {
					if err == nil {
						t.Fatalf("%s = %d accepted, want an error", b.Key, tc.value)
					}
					for _, want := range []string{b.Key, filepath.Base(path)} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("error = %q, want it to name %q", err, want)
						}
					}
					if b.Table == "service" && !strings.Contains(err.Error(), `service "x"`) {
						t.Errorf("error = %q, want it to name the service", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s = %d rejected: %v", b.Key, tc.value, err)
				}
				// The value must have arrived unchanged: not wrapped, not
				// replaced by a default.
				got := configValue(t, cfg, b)
				if got != tc.value {
					t.Errorf("%s = %d loaded as %d", b.Key, tc.value, got)
				}
			})
		}
	}
}

// configValue reads a bounded key back out of a loaded config, as an int64.
func configValue(t *testing.T, cfg Config, b Bound) int64 {
	t.Helper()
	var v reflect.Value
	switch b.Table {
	case "policy":
		v = reflect.ValueOf(cfg.Policy)
	case "service":
		v = reflect.ValueOf(cfg.Services[0])
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Tag.Get("toml") != b.Key {
			continue
		}
		f := v.Field(i)
		if f.Kind() >= reflect.Uint && f.Kind() <= reflect.Uint64 {
			return int64(f.Uint())
		}
		return f.Int()
	}
	t.Fatalf("no field for %s.%s", b.Table, b.Key)
	return 0
}

// The two keys that used to wrap, spelled out, because they are the ones a
// reader of this file will look for.
func TestNegativeUnsignedKeysAreRefused(t *testing.T) {
	for _, body := range []string{
		"[policy]\nvram_floor_mib = -1\n",
		"[policy]\nmin_effect_mib = -1\n",
	} {
		_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", body)})
		if err == nil || !strings.Contains(err.Error(), "must not be negative, got -1") {
			t.Errorf("%q: error = %v, want must not be negative, got -1", strings.TrimSpace(body), err)
		}
	}
	// And the floor on poll_interval.
	_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", "[policy]\npoll_interval = \"1ms\"\n")})
	if err == nil || !strings.Contains(err.Error(), "policy.poll_interval must be at least 1s, got \"1ms\"") {
		t.Errorf("poll_interval = 1ms: error = %v", err)
	}
}

// drain_timeout is the one key with an upper bound. A drain is a client and a
// control connection both waiting on a service that has already been told to
// let go, so a config may not ask them to wait all day.
func TestDrainTimeoutHasAnUpperBound(t *testing.T) {
	head := "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\ndrain_timeout = "
	t.Run("at the maximum", func(t *testing.T) {
		cfg, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", head+`"10m"`+"\n")})
		if err != nil {
			t.Fatalf("10m rejected: %v", err)
		}
		if got := cfg.Services[0].DrainTimeout.D(); got != MaxDrainTimeout {
			t.Errorf("drain_timeout = %s, want %s", got, MaxDrainTimeout)
		}
	})
	for _, value := range []string{`"10m1s"`, `"11m"`, `"24h"`, `"2562047h47m16.854775807s"`} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", head+value+"\n")})
			if err == nil {
				t.Fatalf("drain_timeout = %s accepted, want a refusal", value)
			}
			for _, want := range []string{`service "x": drain_timeout must be at most 10m0s`, "c.toml"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
	// No other key gains an upper bound by accident.
	for _, b := range NumericBounds {
		if b.Max != 0 && b.Key != "drain_timeout" && b.Key != "timeout" {
			t.Errorf("%s.%s has an upper bound of %d, which is undocumented", b.Table, b.Key, b.Max)
		}
	}
}

// timeout is bounded too. Without an upper bound the binary accepted a
// timeout so large that timeout plus drain_timeout overflowed an int64, and
// the plan bound built from that sum wrapped negative: the daemon then
// announced no bound at all and the client fell back to its fixed 90 second
// wait, which is the opposite of what a long timeout asks for.
func TestTimeoutHasAnUpperBound(t *testing.T) {
	head := "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\ntimeout = "
	t.Run("at the maximum", func(t *testing.T) {
		cfg, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", head+`"1h"`+"\n")})
		if err != nil {
			t.Fatalf("1h rejected: %v", err)
		}
		if got := cfg.Services[0].Timeout.D(); got != MaxServiceTimeout {
			t.Errorf("timeout = %s, want %s", got, MaxServiceTimeout)
		}
	})
	// The last value is the one the binary used to accept, at which the sum
	// with drain_timeout overflows.
	for _, value := range []string{`"1h0m1s"`, `"2h"`, `"24h"`, `"2562047h47m16s"`} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", head+value+"\n")})
			if err == nil {
				t.Fatalf("timeout = %s accepted, want a refusal", value)
			}
			for _, want := range []string{`service "x": timeout must be at most 1h0m0s`, "c.toml"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// serviceHead is a minimal, valid [[service]] block for the literal tests to
// hang one key off.
const serviceHead = "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\n"

// writeKey writes one file whose only interesting content is key = literal,
// spelled exactly as given so that a raw TOML spelling can be tested.
func writeKey(t *testing.T, b Bound, literal string) string {
	t.Helper()
	body := fmt.Sprintf("[policy]\n%s = %s\n", b.Key, literal)
	if b.Table == "service" {
		body = serviceHead + fmt.Sprintf("%s = %s\n", b.Key, literal)
	}
	return writeFile(t, t.TempDir(), "c.toml", body)
}

// loadKey loads a file that sets one key to one literal and reports either
// the value it arrived as or the error.
func loadKey(t *testing.T, b Bound, literal string) (int64, error) {
	t.Helper()
	cfg, err := LoadFrom([]string{writeKey(t, b, literal)})
	if err != nil {
		return 0, err
	}
	return configValue(t, cfg, b), nil
}

// A rejection has to name the key and the file, or an operator cannot find
// what to fix. Service keys name the service too.
func assertRejected(t *testing.T, b Bound, literal string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s = %s was accepted, want a refusal", b.Key, literal)
	}
	for _, want := range []string{b.Key, "c.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %s: error %q does not name %q", b.Key, literal, err, want)
		}
	}
}

// A duration key takes a duration string and nothing else. A bare number used
// to slip past the range check entirely: TOML hands an integer to a
// TextUnmarshaler as its digits, time.ParseDuration accepts a unitless "0",
// and Validate then replaced the zero with the default. poll_interval = 0
// started a daemon at 5s while the operator believed they had set it.
func TestDurationKeysTakeOnlyDurationStrings(t *testing.T) {
	// Spellings a file can write that are not a duration string. Every one
	// must be refused, naming the key and the file.
	refused := []string{
		"0", "-0", "5", "-1", "1", "9223372036854775807",
		"0x10", "1_000", "+5", "0b101", "0o17",
		"1e3", "5.0", "-0.0",
		"true", "false",
		`"5"`, `"0"`, `"0s"`, `"-0s"`, `"-1s"`, `"1e3s"`, `"0x10s"`, `"1_000s"`, `""`,
		"[]", "{}",
	}
	for _, b := range NumericBounds {
		if !b.Duration {
			continue
		}
		for _, literal := range refused {
			t.Run(b.Key+"/"+literal, func(t *testing.T) {
				_, err := loadKey(t, b, literal)
				assertRejected(t, b, literal, err)
			})
		}
	}
}

// The bare zero, spelled out on all four keys at once, because it is the one
// this test file exists for: it loaded, and the daemon then ran on defaults.
func TestBareZeroOnEveryDurationKeyIsRefused(t *testing.T) {
	for _, body := range []string{
		"[policy]\npoll_interval = 0\n",
		"[policy]\npoll_interval = -0\n",
		"[policy]\naction_cooldown = 0\n",
		"[policy]\naction_cooldown = -0\n",
		serviceHead + "timeout = 0\n",
		serviceHead + "timeout = -0\n",
		serviceHead + "drain_timeout = 0\n",
		serviceHead + "drain_timeout = -0\n",
	} {
		t.Run(strings.TrimSpace(strings.ReplaceAll(body, "\n", " ")), func(t *testing.T) {
			_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", body)})
			if err == nil {
				t.Fatal("a bare zero was accepted, want a refusal")
			}
			if want := `must be a duration string such as "5s"`; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		})
	}
}

// The values a file may set must arrive unchanged. A file that sets a key and
// gets the default instead is the failure this whole check exists to stop.
func TestDurationKeysAcceptedValuesSurviveValidate(t *testing.T) {
	type want struct {
		literal string
		value   time.Duration
	}
	cases := map[string][]want{
		// poll_interval has a floor of 1s, so sub second values are refused.
		"poll_interval":   {{`"1s"`, time.Second}, {`"2562047h47m16.854775807s"`, 1<<63 - 1}},
		"action_cooldown": {{`"1ns"`, time.Nanosecond}, {`"999ms"`, 999 * time.Millisecond}, {`"1s"`, time.Second}},
		"timeout":         {{`"1ns"`, time.Nanosecond}, {`"999ms"`, 999 * time.Millisecond}, {`"1h"`, time.Hour}},
		"drain_timeout":   {{`"1ns"`, time.Nanosecond}, {`"999ms"`, 999 * time.Millisecond}, {`"10m"`, 10 * time.Minute}},
	}
	for _, b := range NumericBounds {
		if !b.Duration {
			continue
		}
		for _, w := range cases[b.Key] {
			t.Run(b.Key+"/"+w.literal, func(t *testing.T) {
				got, err := loadKey(t, b, w.literal)
				if err != nil {
					t.Fatalf("%s = %s rejected: %v", b.Key, w.literal, err)
				}
				if time.Duration(got) != w.value {
					t.Errorf("%s = %s loaded as %s, want %s", b.Key, w.literal, time.Duration(got), w.value)
				}
			})
		}
	}
	// 999ms is under a second and is legal on every duration key but the
	// poll interval, which probes every service each time round.
	if _, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", "[policy]\npoll_interval = \"999ms\"\n")}); err == nil {
		t.Error("poll_interval = \"999ms\" accepted, want the 1s floor to refuse it")
	}
}

// The integer keys take integers, in any spelling TOML has, and nothing else.
func TestIntegerKeysAcrossEverySpelling(t *testing.T) {
	accepted := []struct {
		literal string
		value   int64
	}{
		{"0", 0}, {"5", 5}, {"+5", 5}, {"0x10", 16}, {"1_000", 1000},
		{"0b101", 5}, {"0o17", 15}, {"9223372036854775807", 1<<63 - 1},
	}
	refused := []string{
		"-1", "1e3", "5.0", "-0.0", `"5"`, `"0"`, `""`, "true", "[]", "{}",
		"9223372036854775808", "-9223372036854775809", "18446744073709551615",
	}
	for _, b := range NumericBounds {
		if b.Duration || b.Signed {
			continue
		}
		for _, tc := range accepted {
			t.Run(b.Key+"/"+tc.literal, func(t *testing.T) {
				got, err := loadKey(t, b, tc.literal)
				if err != nil {
					t.Fatalf("%s = %s rejected: %v", b.Key, tc.literal, err)
				}
				if got != tc.value {
					t.Errorf("%s = %s loaded as %d, want %d", b.Key, tc.literal, got, tc.value)
				}
			})
		}
		for _, literal := range refused {
			t.Run(b.Key+"/"+literal, func(t *testing.T) {
				_, err := loadKey(t, b, literal)
				assertRejected(t, b, literal, err)
			})
		}
	}
}

// priority is the one key that is negative by design, so it has no range: it
// takes anything an int64 holds, and nothing wider. Its bound therefore
// declares no Min or Max, because neither was ever read.
func TestPriorityTakesTheWholeInt64Range(t *testing.T) {
	var priority Bound
	for _, b := range NumericBounds {
		if b.Signed {
			if priority.Key != "" {
				t.Fatalf("more than one signed bound: %s and %s", priority.Key, b.Key)
			}
			priority = b
		}
	}
	if priority.Key != "priority" {
		t.Fatalf("the signed bound is %q, want priority", priority.Key)
	}
	if priority.Min != 0 || priority.Max != 0 {
		t.Errorf("priority declares Min %d, Max %d, neither of which is enforced for a signed key",
			priority.Min, priority.Max)
	}
	for _, tc := range []struct {
		literal string
		value   int64
	}{
		{"-9223372036854775808", math.MinInt64},
		{"9223372036854775807", math.MaxInt64},
		{"-1", -1}, {"0", 0}, {"+5", 5}, {"0x10", 16},
	} {
		t.Run("accepted/"+tc.literal, func(t *testing.T) {
			got, err := loadKey(t, priority, tc.literal)
			if err != nil {
				t.Fatalf("priority = %s rejected: %v", tc.literal, err)
			}
			if got != tc.value {
				t.Errorf("priority = %s loaded as %d, want %d", tc.literal, got, tc.value)
			}
		})
	}
	for _, literal := range []string{"9223372036854775808", "-9223372036854775809", "1e3", `"5"`, "true"} {
		t.Run("refused/"+literal, func(t *testing.T) {
			_, err := loadKey(t, priority, literal)
			assertRejected(t, priority, literal, err)
		})
	}
}

// Bounds are checked per file as each is merged, so neither ordering of a
// legal and an illegal file can load: the illegal one is named either way.
func TestLayeringRefusesTheOffendingFileInEitherOrder(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", "[policy]\nvram_floor_mib = 100\n")
	bad := writeFile(t, dir, "overlay.toml", "[policy]\npoll_interval = 0\n")
	legal := writeFile(t, dir, "legal.toml", "[policy]\nvram_floor_mib = 200\n")

	for _, order := range [][]string{{base, bad}, {bad, base}} {
		if _, err := LoadFrom(order); err == nil {
			t.Errorf("%v loaded, want the bare zero to be refused", order)
		} else if !strings.Contains(err.Error(), "overlay.toml") {
			t.Errorf("%v: error %q does not name overlay.toml", order, err)
		}
	}
	// Two legal files still layer last wins, so the refusal above is about
	// the value and not about layering itself.
	for _, tc := range []struct {
		order []string
		want  uint64
	}{
		{[]string{legal, base}, 100},
		{[]string{base, legal}, 200},
	} {
		cfg, err := LoadFrom(tc.order)
		if err != nil {
			t.Fatalf("%v: %v", tc.order, err)
		}
		if cfg.Policy.VRAMFloorMiB != tc.want {
			t.Errorf("%v gave vram_floor_mib %d, want %d", tc.order, cfg.Policy.VRAMFloorMiB, tc.want)
		}
	}
}

// A service block with no name yet cannot be called by name, so the error
// says which block it is instead.
func TestABoundInAnUnnamedServiceBlockIsLabelledByPosition(t *testing.T) {
	body := "[[service]]\nname = \"first\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\n" +
		"\n[[service]]\nadapter = \"ollama\"\nendpoint = \"http://h:2\"\ndrain_timeout = 0\n"
	_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", body)})
	if err == nil {
		t.Fatal("a bare zero in an unnamed service block was accepted")
	}
	if want := "[[service]] #2: drain_timeout"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// The whole point of checking the raw value: nothing a file sets is ever
// quietly replaced by a default.
func TestNothingAFileSetsIsReplacedByADefault(t *testing.T) {
	body := "[policy]\npoll_interval = \"7s\"\naction_cooldown = \"11s\"\nvram_floor_mib = 0\nmin_effect_mib = 0\ngpu_index = 0\n" +
		serviceHead + "timeout = \"3s\"\ndrain_timeout = \"4s\"\npriority = 0\n"
	cfg, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", body)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key       string
		got, want any
	}{
		{"poll_interval", cfg.Policy.PollInterval.D(), 7 * time.Second},
		{"action_cooldown", cfg.Policy.ActionCooldown.D(), 11 * time.Second},
		{"vram_floor_mib", cfg.Policy.VRAMFloorMiB, uint64(0)},
		{"min_effect_mib", cfg.Policy.MinEffectMiB, uint64(0)},
		{"timeout", cfg.Services[0].Timeout.D(), 3 * time.Second},
		{"drain_timeout", cfg.Services[0].DrainTimeout.D(), 4 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v: a value the file set was replaced", tc.key, tc.got, tc.want)
		}
	}
	// A key the file leaves out still gets its default, which is the only
	// substitution there should be.
	unset, err := LoadFrom([]string{writeFile(t, t.TempDir(), "d.toml", serviceHead)})
	if err != nil {
		t.Fatal(err)
	}
	if got := unset.Services[0].Timeout.D(); got != DefaultServiceTimeout {
		t.Errorf("an unset timeout loaded as %s, want the %s default", got, DefaultServiceTimeout)
	}
	if got := unset.Services[0].DrainTimeout.D(); got != DefaultDrainTimeout {
		t.Errorf("an unset drain_timeout loaded as %s, want the %s default", got, DefaultDrainTimeout)
	}
}
