package config

import (
	"strings"
	"testing"
)

// A unit name reaches a systemctl argument and every error message about the
// service. TOML 1.1, which the parser now accepts, decodes \e, so a config
// file can put an escape character into one, where it is invisible in both.
func TestUnitNamesAreValidated(t *testing.T) {
	accepted := []string{
		"ollama.service",
		"comfyui.service",
		"my-app.service",
		"my_app.service",
		"app@instance.service",
		"dev-disk-by\\x2duuid.device",
		"home.mount",
		"backup.timer",
		"machine.slice",
		"session-1.scope",
		"a.path",
		"x.socket",
		"y.target",
		"z.swap",
		"proc-sys.automount",
		"a.b.c.service",
		"UPPER123.service",
		strings.Repeat("a", 255-len(".service")) + ".service",
	}
	for _, unit := range accepted {
		t.Run("accepted/"+unit, func(t *testing.T) {
			if err := validateUnit(unit); err != nil {
				t.Errorf("validateUnit(%q) = %v, want it accepted", unit, err)
			}
		})
	}

	refused := []struct {
		name, unit, want string
	}{
		{"no type suffix", "ollama", "must end in a unit type"},
		{"an unknown type suffix", "ollama.svc", "must end in a unit type"},
		{"nothing but a suffix", ".service", "must end in a unit type"},
		{"an escape character", "a\x1bb.service", "must match"},
		{"a newline", "a\nb.service", "must match"},
		{"a null byte", "a\x00b.service", "must match"},
		{"a space", "my app.service", "must match"},
		{"a slash", "a/b.service", "must match"},
		{"a semicolon", "a;systemctl.service", "must match"},
		{"a leading dash that reads as a flag", "--user.service", "must match"},
		{"over systemd's length limit", strings.Repeat("a", 249) + ".service", "over systemd's 255 byte limit"},
	}
	for _, tt := range refused {
		t.Run("refused/"+tt.name, func(t *testing.T) {
			err := validateUnit(tt.unit)
			if err == nil {
				t.Fatalf("validateUnit(%q) was accepted, want a refusal", tt.unit)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A control character must be visible in the message that refuses it. An
// error naming a unit that looks identical to the legal one helps nobody.
func TestARefusedUnitShowsItsControlCharacters(t *testing.T) {
	err := validateUnit("oll\x1bama.service")
	if err == nil {
		t.Fatal("an escape character was accepted")
	}
	if !strings.Contains(err.Error(), `oll\x1bama.service`) {
		t.Errorf("error = %q, want the escape shown as \\x1b", err)
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("error = %q, want no raw escape character in it", err)
	}
}

// The refusal has to arrive through a config file, naming the service, or it
// is a function nothing calls.
func TestAConfigFileWithABadUnitIsRefused(t *testing.T) {
	// \e is TOML 1.1, which the parser accepts as of this release, so this
	// is a file a user can now actually write.
	body := "[[service]]\nname = \"oll\"\nadapter = \"systemd-unit\"\nunit = \"oll\\eama.service\"\n"
	_, err := LoadFrom([]string{writeFile(t, t.TempDir(), "c.toml", body)})
	if err == nil {
		t.Fatal("a unit holding an escape character was accepted")
	}
	for _, want := range []string{`service "oll"`, "must match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}

	// The ordinary case still loads.
	good := "[[service]]\nname = \"oll\"\nadapter = \"systemd-unit\"\nunit = \"ollama.service\"\n"
	cfg, err := LoadFrom([]string{writeFile(t, t.TempDir(), "d.toml", good)})
	if err != nil {
		t.Fatalf("a plain unit was refused: %v", err)
	}
	if cfg.Services[0].Unit != "ollama.service" {
		t.Errorf("unit = %q, want ollama.service", cfg.Services[0].Unit)
	}
}
