// Package config loads, merges and validates gpu-bouncer configuration.
//
// Configuration is TOML. Two locations are consulted, in order:
//
//	/etc/gpu-bouncer/config.toml        (system)
//	$XDG_CONFIG_HOME/gpu-bouncer/config.toml   (user, defaults to ~/.config)
//
// A file later in that list is layered on top of an earlier one: policy keys
// that the later file explicitly sets win, and services are matched by name so
// a user file can retune one service without restating the rest. Setting
// GPU_BOUNCER_CONFIG makes that single file the only one loaded.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// AdapterKind names the driver used to talk to a service.
type AdapterKind string

const (
	AdapterOllama      AdapterKind = "ollama"
	AdapterComfyUI     AdapterKind = "comfyui"
	AdapterLlamaSwap   AdapterKind = "llama-swap"
	AdapterSystemdUnit AdapterKind = "systemd-unit"
)

// KnownAdapters lists every adapter kind accepted in a config file.
func KnownAdapters() []AdapterKind {
	return []AdapterKind{AdapterOllama, AdapterComfyUI, AdapterLlamaSwap, AdapterSystemdUnit}
}

func (k AdapterKind) valid() bool {
	for _, known := range KnownAdapters() {
		if k == known {
			return true
		}
	}
	return false
}

// needsEndpoint reports whether the adapter talks over HTTP.
func (k AdapterKind) needsEndpoint() bool {
	return k == AdapterOllama || k == AdapterComfyUI || k == AdapterLlamaSwap
}

// Duration is a time.Duration that decodes from a TOML string such as "5s".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Service is one AI service that gpu-bouncer is allowed to observe, and in
// some cases act on. Services that are absent from the config are invisible to
// gpu-bouncer and are never touched.
type Service struct {
	// Name identifies the service on the command line and in logs.
	Name string `toml:"name"`
	// Adapter selects the driver. See KnownAdapters.
	Adapter AdapterKind `toml:"adapter"`
	// Endpoint is the base URL for HTTP adapters, for example
	// "http://127.0.0.1:11434".
	Endpoint string `toml:"endpoint"`
	// Unit is the systemd unit name for the systemd-unit adapter.
	Unit string `toml:"unit"`
	// UserUnit selects `systemctl --user` rather than the system manager.
	UserUnit bool `toml:"user_unit"`
	// Priority orders services against each other. Higher wins. Services with
	// equal priority never evict each other.
	Priority int `toml:"priority"`
	// AllowStop must be true before gpu-bouncer may take any process-level
	// action (stopping a unit). Graceful adapter calls that only ask a service
	// to drop its models do not require it.
	AllowStop bool `toml:"allow_stop"`
	// Timeout bounds every request made to this service.
	Timeout Duration `toml:"timeout"`
	// DrainTimeout bounds how long a release waits for the service to
	// confirm that what it released is gone. Only the ollama adapter waits.
	DrainTimeout Duration `toml:"drain_timeout"`
}

// Policy holds the daemon-wide arbitration settings.
type Policy struct {
	// VRAMFloorMiB is the amount of free VRAM the daemon tries to keep
	// available. Reactive mode engages when free VRAM drops below it.
	VRAMFloorMiB uint64 `toml:"vram_floor_mib"`
	// DefaultWorkload names the service that is assumed to want the GPU when
	// no explicit request is outstanding. Reactive mode protects it. Empty
	// means reactive mode protects whichever configured service is up and has
	// the highest priority.
	DefaultWorkload string `toml:"default_workload"`
	// Reactive enables acting without an explicit request.
	Reactive bool `toml:"reactive"`
	// PollInterval is how often the daemon samples VRAM and services.
	PollInterval Duration `toml:"poll_interval"`
	// GPUIndex selects which GPU to arbitrate. v0.1 arbitrates one GPU.
	GPUIndex int `toml:"gpu_index"`
	// MinEffectMiB is the smallest measured gain in free VRAM that counts as
	// an action having worked. An action that gains less puts its service
	// into a cooldown, so a service that reloads the moment it is released
	// is not released once per poll forever.
	MinEffectMiB uint64 `toml:"min_effect_mib"`
	// ActionCooldown is how long reactive plans leave a service alone after
	// an action on it had no effect. Explicit request and evict bypass it.
	ActionCooldown Duration `toml:"action_cooldown"`
}

// Config is a fully merged and validated configuration.
type Config struct {
	Policy   Policy    `toml:"policy"`
	Services []Service `toml:"service"`

	// Sources lists the files that contributed, in load order. It is not
	// itself settable from a config file.
	Sources []string `toml:"-"`
}

// Defaults returns the configuration used when no file exists anywhere. It is
// deliberately inert: no services means nothing is ever acted on.
func Defaults() Config {
	return Config{
		Policy: Policy{
			VRAMFloorMiB:   512,
			Reactive:       false,
			PollInterval:   Duration(5 * time.Second),
			GPUIndex:       0,
			MinEffectMiB:   64,
			ActionCooldown: Duration(60 * time.Second),
		},
	}
}

// DefaultServiceTimeout bounds a service request when the config does not.
const DefaultServiceTimeout = 5 * time.Second

// DefaultDrainTimeout bounds a release's wait for the service to confirm the
// unload when the config does not.
const DefaultDrainTimeout = 30 * time.Second

const (
	systemPath  = "/etc/gpu-bouncer/config.toml"
	userRelPath = "gpu-bouncer/config.toml"
	// EnvConfig overrides path discovery with a single file.
	EnvConfig = "GPU_BOUNCER_CONFIG"
)

// SearchPaths returns the config files that Load will consult, in load order.
// Files that do not exist are still listed; Load skips them.
func SearchPaths() []string {
	if override := os.Getenv(EnvConfig); override != "" {
		return []string{override}
	}
	paths := []string{systemPath}
	if dir := userConfigDir(); dir != "" {
		paths = append(paths, filepath.Join(dir, userRelPath))
	}
	return paths
}

func userConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// Load reads and merges every config file in SearchPaths that exists, then
// validates the result. A missing file is not an error; no files at all yields
// Defaults with an empty service list, which is valid and inert.
//
// When GPU_BOUNCER_CONFIG is set, that file must exist.
func Load() (Config, error) {
	return LoadFrom(SearchPaths())
}

// LoadFrom is Load against an explicit list of paths, in load order.
func LoadFrom(paths []string) (Config, error) {
	cfg := Defaults()
	explicit := os.Getenv(EnvConfig) != ""
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			if explicit {
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			}
			continue
		}
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
		if err := mergeFile(&cfg, path, data); err != nil {
			return Config{}, err
		}
		cfg.Sources = append(cfg.Sources, path)
	}
	if err := Validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// mergeFile decodes one file and layers it onto cfg.
//
// The file is decoded twice: once into the typed Config for values, and once
// into a generic map so that we know exactly which keys each block set.
// toml.MetaData cannot answer that for [[service]] blocks, because it reports
// array-of-table keys without their index.
func mergeFile(cfg *Config, path string, data []byte) error {
	var layer Config
	md, err := toml.Decode(string(data), &layer)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	rawServices := serviceBlocks(raw)
	// A misspelled service key is reported against the service it sits in.
	// toml.MetaData cannot say which [[service]] block an undecoded key came
	// from, but the raw decode can.
	for i, block := range rawServices {
		var unknown []string
		for k := range block {
			if _, known := serviceKeys[k]; !known {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			slices.Sort(unknown)
			label := fmt.Sprintf("[[service]] #%d", i+1)
			if i < len(layer.Services) && layer.Services[i].Name != "" {
				label = fmt.Sprintf("service %q", layer.Services[i].Name)
			}
			return fmt.Errorf("config %s: %s: unknown key(s): %s", path, label, strings.Join(unknown, ", "))
		}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		slices.Sort(keys)
		return fmt.Errorf("config %s: unknown key(s): %s", path, strings.Join(slices.Compact(keys), ", "))
	}

	policyKeys := subKeys(raw, "policy")
	if _, ok := policyKeys["vram_floor_mib"]; ok {
		cfg.Policy.VRAMFloorMiB = layer.Policy.VRAMFloorMiB
	}
	if _, ok := policyKeys["default_workload"]; ok {
		cfg.Policy.DefaultWorkload = layer.Policy.DefaultWorkload
	}
	if _, ok := policyKeys["reactive"]; ok {
		cfg.Policy.Reactive = layer.Policy.Reactive
	}
	if _, ok := policyKeys["poll_interval"]; ok {
		if layer.Policy.PollInterval <= 0 {
			return fmt.Errorf("config %s: policy.poll_interval must be a positive duration, got %q", path, layer.Policy.PollInterval.D())
		}
		cfg.Policy.PollInterval = layer.Policy.PollInterval
	}
	if _, ok := policyKeys["gpu_index"]; ok {
		cfg.Policy.GPUIndex = layer.Policy.GPUIndex
	}
	if _, ok := policyKeys["min_effect_mib"]; ok {
		cfg.Policy.MinEffectMiB = layer.Policy.MinEffectMiB
	}
	if _, ok := policyKeys["action_cooldown"]; ok {
		if layer.Policy.ActionCooldown <= 0 {
			return fmt.Errorf("config %s: policy.action_cooldown must be a positive duration, got %q", path, layer.Policy.ActionCooldown.D())
		}
		cfg.Policy.ActionCooldown = layer.Policy.ActionCooldown
	}

	inThisFile := make(map[string]struct{}, len(layer.Services))
	for i, svc := range layer.Services {
		if svc.Name == "" {
			return fmt.Errorf("config %s: [[service]] #%d has no name", path, i+1)
		}
		if _, dup := inThisFile[svc.Name]; dup {
			return fmt.Errorf("config %s: service %q defined twice in the same file", path, svc.Name)
		}
		inThisFile[svc.Name] = struct{}{}
		var keys map[string]struct{}
		if i < len(rawServices) {
			keys = rawServices[i]
		}
		// A duration a file sets explicitly must be positive. Silently
		// replacing "0s" with the default would be exactly the kind of quiet
		// change of behaviour that unknown keys are rejected to prevent.
		for key, value := range map[string]Duration{"timeout": svc.Timeout, "drain_timeout": svc.DrainTimeout} {
			if _, set := keys[key]; set && value <= 0 {
				return fmt.Errorf("config %s: service %q: %s must be a positive duration, got %q", path, svc.Name, key, value.D())
			}
		}
		if idx := indexOfService(cfg.Services, svc.Name); idx >= 0 {
			mergeService(&cfg.Services[idx], svc, keys)
			continue
		}
		cfg.Services = append(cfg.Services, svc)
	}
	return nil
}

// serviceKeys is every key a [[service]] block may set. It mirrors the toml
// tags on Service and is what the per service unknown key error checks.
var serviceKeys = map[string]struct{}{
	"name": {}, "adapter": {}, "endpoint": {}, "unit": {}, "user_unit": {},
	"priority": {}, "allow_stop": {}, "timeout": {}, "drain_timeout": {},
}

// subKeys returns the set of keys present in the named top level table.
func subKeys(raw map[string]any, table string) map[string]struct{} {
	out := map[string]struct{}{}
	sub, ok := raw[table].(map[string]any)
	if !ok {
		return out
	}
	for k := range sub {
		out[k] = struct{}{}
	}
	return out
}

// serviceBlocks returns, per [[service]] block in file order, the set of keys
// that block set explicitly.
func serviceBlocks(raw map[string]any) []map[string]struct{} {
	list, ok := raw["service"].([]map[string]any)
	if !ok {
		anyList, isAny := raw["service"].([]any)
		if !isAny {
			return nil
		}
		for _, item := range anyList {
			m, isMap := item.(map[string]any)
			if !isMap {
				return nil
			}
			list = append(list, m)
		}
	}
	out := make([]map[string]struct{}, 0, len(list))
	for _, block := range list {
		keys := make(map[string]struct{}, len(block))
		for k := range block {
			keys[k] = struct{}{}
		}
		out = append(out, keys)
	}
	return out
}

// mergeService overlays only the fields this block set explicitly, so a user
// file can retune one key of a system defined service without restating it.
func mergeService(dst *Service, src Service, keys map[string]struct{}) {
	set := func(key string) bool {
		_, ok := keys[key]
		return ok
	}
	if set("adapter") {
		dst.Adapter = src.Adapter
	}
	if set("endpoint") {
		dst.Endpoint = src.Endpoint
	}
	if set("unit") {
		dst.Unit = src.Unit
	}
	if set("user_unit") {
		dst.UserUnit = src.UserUnit
	}
	if set("priority") {
		dst.Priority = src.Priority
	}
	if set("allow_stop") {
		dst.AllowStop = src.AllowStop
	}
	if set("timeout") {
		dst.Timeout = src.Timeout
	}
	if set("drain_timeout") {
		dst.DrainTimeout = src.DrainTimeout
	}
}

func indexOfService(services []Service, name string) int {
	for i := range services {
		if services[i].Name == name {
			return i
		}
	}
	return -1
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate checks a merged config and fills in per-service defaults. It is
// exported so that tests and `gpu-bouncer plan` can validate a hand-built
// config without touching the filesystem.
func Validate(cfg *Config) error {
	// These can only be non positive in a Config built in code without going
	// through Defaults; a file that sets them so is refused in mergeFile.
	if cfg.Policy.PollInterval <= 0 {
		cfg.Policy.PollInterval = Duration(5 * time.Second)
	}
	if cfg.Policy.ActionCooldown <= 0 {
		cfg.Policy.ActionCooldown = Duration(60 * time.Second)
	}
	if cfg.Policy.GPUIndex < 0 {
		return fmt.Errorf("policy.gpu_index must not be negative, got %d", cfg.Policy.GPUIndex)
	}

	seen := make(map[string]struct{}, len(cfg.Services))
	for i := range cfg.Services {
		svc := &cfg.Services[i]
		if !nameRE.MatchString(svc.Name) {
			return fmt.Errorf("service %q: name must match %s", svc.Name, nameRE)
		}
		if _, dup := seen[svc.Name]; dup {
			return fmt.Errorf("service %q: defined twice", svc.Name)
		}
		seen[svc.Name] = struct{}{}

		if !svc.Adapter.valid() {
			return fmt.Errorf("service %q: unknown adapter %q (known: %s)",
				svc.Name, svc.Adapter, joinAdapters())
		}
		if svc.Timeout <= 0 {
			svc.Timeout = Duration(DefaultServiceTimeout)
		}
		if svc.DrainTimeout <= 0 {
			svc.DrainTimeout = Duration(DefaultDrainTimeout)
		}

		if svc.Adapter.needsEndpoint() {
			if svc.Endpoint == "" {
				return fmt.Errorf("service %q: adapter %q requires an endpoint", svc.Name, svc.Adapter)
			}
			if err := validateEndpoint(svc.Endpoint); err != nil {
				return fmt.Errorf("service %q: %w", svc.Name, err)
			}
			if svc.Unit != "" {
				return fmt.Errorf("service %q: adapter %q does not take a unit", svc.Name, svc.Adapter)
			}
		}
		if svc.Adapter == AdapterSystemdUnit {
			if svc.Unit == "" {
				return fmt.Errorf("service %q: adapter %q requires a unit", svc.Name, svc.Adapter)
			}
			if svc.Endpoint != "" {
				return fmt.Errorf("service %q: adapter %q does not take an endpoint", svc.Name, svc.Adapter)
			}
		}
		svc.Endpoint = strings.TrimRight(svc.Endpoint, "/")
	}

	if wl := cfg.Policy.DefaultWorkload; wl != "" {
		if _, ok := seen[wl]; !ok {
			return fmt.Errorf("policy.default_workload %q names no configured service", wl)
		}
	}
	return nil
}

func validateEndpoint(endpoint string) error {
	scheme, _, found := strings.Cut(endpoint, "://")
	if !found || (scheme != "http" && scheme != "https") {
		return fmt.Errorf("endpoint %q must use http or https, for example http://127.0.0.1:11434", endpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has no host", endpoint)
	}
	if u.User != nil {
		// A password here would be sent as Basic auth and would surface in
		// every error string. No adapter authenticates through the URL.
		return fmt.Errorf("endpoint %q carries a username or password, which gpu-bouncer never sends; for a llama-swap API key set GPU_BOUNCER_LLAMA_SWAP_API_KEY in the daemon's environment",
			u.Redacted())
	}
	return nil
}

func joinAdapters() string {
	names := make([]string, 0, len(KnownAdapters()))
	for _, k := range KnownAdapters() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

// Service returns the named service, or false.
func (c Config) Service(name string) (Service, bool) {
	if i := indexOfService(c.Services, name); i >= 0 {
		return c.Services[i], true
	}
	return Service{}, false
}
