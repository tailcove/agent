package kube

import (
	"regexp"
	"strings"
)

// replicaSetSuffix is the hash a Deployment adds to the name of its ReplicaSet.
var replicaSetSuffix = regexp.MustCompile(`-[0-9a-f]{6,10}$`)

// WorkloadName is what to call the owner of a pod: a ReplicaSet is named after
// its Deployment plus a hash, so the hash is taken off; a Job made by a CronJob
// is named after it plus a number, so that is too. Anything else keeps its name.
func WorkloadName(kind, name string) string {
	switch kind {
	case "ReplicaSet":
		return replicaSetSuffix.ReplaceAllString(name, "")
	case "Job":
		if cut := strings.LastIndex(name, "-"); cut > 0 && isDigits(name[cut+1:]) && len(name)-cut-1 >= 6 {
			return name[:cut]
		}
	}

	return name
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
