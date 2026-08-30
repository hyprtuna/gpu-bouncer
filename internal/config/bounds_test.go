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
	serviceHead := "[[service]]\nname = \"x\"\nadapter = \"ollama\"\nendpoint = \"http://h:1\"\n"
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
		if b.Max != 0 && b.Key != "drain_timeout" {
			t.Errorf("%s.%s has an upper bound of %d, which is undocumented", b.Table, b.Key, b.Max)
		}
	}
}
