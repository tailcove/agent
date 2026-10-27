// Package kube maps the container logs a Kubernetes node keeps on disk to the
// pods they belong to: what to follow, which pod a file is, and where its lines
// should go.
package kube

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PodLog is what the path of a container log file says: the kubelet writes
// <pods dir>/<namespace>_<pod>_<uid>/<container>/<restart count>.log.
type PodLog struct {
	Namespace string
	Pod       string
	UID       string
	Container string
	Restarts  int
}

// ParsePodLog reads the path of a container log under podsDir. A path that is
// not one (the wrong depth, a name that is not <ns>_<pod>_<uid>, a file that
// is not N.log such as a rotated 0.log.20261027-101500) is not a pod log.
func ParsePodLog(podsDir, path string) (PodLog, bool) {
	relative, err := filepath.Rel(filepath.Clean(podsDir), filepath.Clean(path))
	if err != nil || strings.HasPrefix(relative, "..") {
		return PodLog{}, false
	}

	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 {
		return PodLog{}, false
	}

	owner := strings.Split(parts[0], "_")
	if len(owner) != 3 || owner[0] == "" || owner[1] == "" || owner[2] == "" || parts[1] == "" {
		return PodLog{}, false
	}

	number, found := strings.CutSuffix(parts[2], ".log")
	if !found {
		return PodLog{}, false
	}

	restarts, err := strconv.Atoi(number)
	if err != nil || restarts < 0 || strconv.Itoa(restarts) != number {
		return PodLog{}, false
	}

	return PodLog{Namespace: owner[0], Pod: owner[1], UID: owner[2], Container: parts[1], Restarts: restarts}, true
}

// ContainerLink is what the name of a file in /var/log/containers says:
// <pod>_<namespace>_<container>-<container id>.log, a link the kubelet keeps
// to the running container's log.
type ContainerLink struct {
	Pod         string
	Namespace   string
	Container   string
	ContainerID string
}

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseContainerLink reads the name of a container link.
func ParseContainerLink(name string) (ContainerLink, bool) {
	base, found := strings.CutSuffix(filepath.Base(name), ".log")
	if !found {
		return ContainerLink{}, false
	}

	cut := strings.LastIndex(base, "-")
	if cut < 0 || !containerID.MatchString(base[cut+1:]) {
		return ContainerLink{}, false
	}

	owner := strings.Split(base[:cut], "_")
	if len(owner) != 3 || owner[0] == "" || owner[1] == "" || owner[2] == "" {
		return ContainerLink{}, false
	}

	return ContainerLink{Pod: owner[0], Namespace: owner[1], Container: owner[2], ContainerID: base[cut+1:]}, true
}
