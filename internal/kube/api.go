package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Where a pod finds the cluster's API and its own credentials.
const (
	ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	maxPodListBytes = 32 << 20
	maxPodPages     = 40
	podPageSize     = 250
)

// PodInfo is what the agent keeps of a pod the API listed.
type PodInfo struct {
	Namespace  string
	Name       string
	UID        string
	Labels     map[string]string
	Workload   string
	Containers map[string]ContainerInfo
}

// ContainerInfo is a container of a pod as the API reports it.
type ContainerInfo struct {
	ID       string
	Restarts int
}

// API is the small part of the Kubernetes API the agent uses: the pods of one
// node, read only. It needs the RBAC of the manifests in deploy/agent (get, list
// and watch on pods, nothing else) and nothing it is given can reach a secret.
type API struct {
	// URL is the API server, https://host:port.
	URL string
	// TokenFile holds the service account token. It is read for every request:
	// the kubelet rotates it.
	TokenFile string
	// CAFile is the cluster's certificate authority.
	CAFile string
	// Client is used instead of one built from CAFile (tests).
	Client *http.Client
}

// InCluster is the API as a pod reaches it, or an error when this is not a pod.
func InCluster() (*API, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set: not running in a pod")
	}

	return &API{
		URL:       "https://" + net.JoinHostPort(host, port),
		TokenFile: ServiceAccountDir + "/token",
		CAFile:    ServiceAccountDir + "/ca.crt",
	}, nil
}

func (a *API) client() (*http.Client, error) {
	if a.Client != nil {
		return a.Client, nil
	}

	pool := x509.NewCertPool()

	pem, err := os.ReadFile(a.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster CA: %w", err)
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the cluster CA file holds no certificate")
	}

	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}, nil
}

type podList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []struct {
		Metadata struct {
			Name            string            `json:"name"`
			Namespace       string            `json:"namespace"`
			UID             string            `json:"uid"`
			Labels          map[string]string `json:"labels"`
			OwnerReferences []struct {
				Kind       string `json:"kind"`
				Name       string `json:"name"`
				Controller *bool  `json:"controller"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
		Status struct {
			ContainerStatuses []containerStatus `json:"containerStatuses"`
			InitContainers    []containerStatus `json:"initContainerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type containerStatus struct {
	Name         string `json:"name"`
	ContainerID  string `json:"containerID"`
	RestartCount int    `json:"restartCount"`
}

// PodsOnNode lists the pods scheduled on the node.
func (a *API) PodsOnNode(ctx context.Context, node string) ([]PodInfo, error) {
	client, err := a.client()
	if err != nil {
		return nil, err
	}

	var pods []PodInfo

	next := ""

	for page := 0; page < maxPodPages; page++ {
		query := url.Values{"fieldSelector": {"spec.nodeName=" + node}, "limit": {fmt.Sprint(podPageSize)}}
		if next != "" {
			query.Set("continue", next)
		}

		body, err := a.get(ctx, client, "/api/v1/pods?"+query.Encode())
		if err != nil {
			return nil, err
		}

		var list podList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("reading the pod list: %w", err)
		}

		for _, item := range list.Items {
			info := PodInfo{
				Namespace:  item.Metadata.Namespace,
				Name:       item.Metadata.Name,
				UID:        item.Metadata.UID,
				Labels:     item.Metadata.Labels,
				Containers: map[string]ContainerInfo{},
			}

			for _, owner := range item.Metadata.OwnerReferences {
				if owner.Controller != nil && *owner.Controller {
					info.Workload = WorkloadName(owner.Kind, owner.Name)

					break
				}
			}

			for _, status := range append(append([]containerStatus{}, item.Status.InitContainers...), item.Status.ContainerStatuses...) {
				info.Containers[status.Name] = ContainerInfo{ID: containerIDOf(status.ContainerID), Restarts: status.RestartCount}
			}

			pods = append(pods, info)
		}

		if list.Metadata.Continue == "" {
			return pods, nil
		}

		next = list.Metadata.Continue
	}

	return pods, nil
}

func (a *API) get(ctx context.Context, client *http.Client, path string) ([]byte, error) {
	token, err := os.ReadFile(a.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading the service account token: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("asking the Kubernetes API: %w", err)
	}

	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPodListBytes+1))
	if err != nil {
		return nil, err
	}

	if len(body) > maxPodListBytes {
		return nil, errors.New("the pod list is too large")
	}

	if response.StatusCode != http.StatusOK {
		// The status line only: the body can quote what was asked for.
		return nil, fmt.Errorf("the Kubernetes API answered HTTP %d (does the service account have get and list on pods?)", response.StatusCode)
	}

	return body, nil
}

// containerIDOf takes the runtime off a containerID such as containerd://<id>.
func containerIDOf(value string) string {
	if _, id, found := strings.Cut(value, "://"); found {
		return id
	}

	return value
}
