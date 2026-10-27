//go:build !linux && !darwin

package hostmetrics

// Point is one metric value, ready to send.
type Point struct {
	Name   string
	Value  float64
	Labels map[string]string
}

// Collector has nothing to read on this system; host metrics are for Linux and macOS.
type Collector struct {
	ExcludeIfaces []string
	ExcludeMounts []string
}

// NewCollector returns a collector that reports nothing on this system.
func NewCollector(excludeIfaces, excludeMounts []string) *Collector {
	return &Collector{ExcludeIfaces: excludeIfaces, ExcludeMounts: excludeMounts}
}

// NewCollectorAt returns a collector that reports nothing on this system.
func NewCollectorAt(_ string, excludeIfaces, excludeMounts []string) *Collector {
	return NewCollector(excludeIfaces, excludeMounts)
}

// DefaultExcludedInterfaces are virtual interfaces that would only add series.
var DefaultExcludedInterfaces = []string{"lo", "veth*", "docker*", "br-*", "cali*", "flannel*", "cni*", "virbr*"}

// Collect returns nothing on this system.
func (c *Collector) Collect() []Point { return nil }
