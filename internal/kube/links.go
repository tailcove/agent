package kube

import (
	"os"
	"path/filepath"
)

// Running is a container the kubelet keeps a log link for.
type Running struct {
	Namespace   string
	Pod         string
	Container   string
	ContainerID string
}

// ListRunning reads the container links in dir (/var/log/containers). The link
// names carry the namespace, the pod, the container and the runtime id, which is
// all the metrics need to find a container's cgroup, with no API involved. A
// link of a container that is gone may linger until the kubelet clears it: it
// finds no cgroup and adds nothing.
func ListRunning(dir string) []Running {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var running []Running

	for _, entry := range entries {
		link, ok := ParseContainerLink(filepath.Base(entry.Name()))
		if !ok {
			continue
		}

		running = append(running, Running{Namespace: link.Namespace, Pod: link.Pod, Container: link.Container, ContainerID: link.ContainerID})
	}

	return running
}
