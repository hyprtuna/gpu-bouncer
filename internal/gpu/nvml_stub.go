//go:build !cgo

package gpu

import "errors"

// nvmlBuiltIn says whether this binary can use NVML at all.
const nvmlBuiltIn = false

// openNVML always fails in a build made without cgo. go-nvml binds to
// libnvidia-ml through cgo, so a CGO_ENABLED=0 binary cannot use it and falls
// back to sysfs. Read only commands still work; accuracy drops.
func openNVML() (Source, error) {
	return nil, errors.New("built without cgo, rebuild with CGO_ENABLED=1 for NVIDIA support")
}
