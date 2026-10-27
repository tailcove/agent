//go:build !linux

package hostmetrics

// Point is one metric value, ready to send.
type Point struct {
	Name   string
	Value  float64
	Labels map[string]string
}

// Collector has nothing to read outside Linux; host metrics are Linux-only.
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

// Collect returns nothing outside Linux.
func (c *Collector) Collect() []Point { return nil }
