// Package docker reads the logs of the containers on this host from the Docker
// engine's API. The socket is root-equivalent for whoever can write to it, so
// this client can only do three reads, with an identifier it checked: it lists
// containers, inspects one, and streams one's logs. It never starts, stops,
// creates, executes or deletes anything.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultSocket is where the engine listens.
const DefaultSocket = "/var/run/docker.sock"

var containerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// Container is what the agent needs to know of a running container.
type Container struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string
	Tty    bool
}

// Client talks to the engine over its unix socket.
type Client struct {
	Socket string

	http *http.Client
}

// NewClient returns a client for the socket.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}

	return &Client{Socket: socket, http: &http.Client{Transport: transport}}
}

// get issues a GET. It is the only method this client has.
func (c *Client) get(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	address := "http://docker" + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		_ = response.Body.Close()

		return nil, fmt.Errorf("docker answered %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	return response, nil
}

// List returns the running containers.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	response, err := c.get(ctx, "/containers/json", nil)
	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()

	var listed []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(&listed); err != nil {
		return nil, fmt.Errorf("docker: reading the container list: %w", err)
	}

	containers := make([]Container, 0, len(listed))

	for _, one := range listed {
		if !containerID.MatchString(one.ID) {
			continue
		}

		name := ""
		if len(one.Names) > 0 {
			name = strings.TrimPrefix(one.Names[0], "/")
		}

		containers = append(containers, Container{ID: one.ID, Name: name, Image: one.Image, Labels: one.Labels})
	}

	return containers, nil
}

// Tty says whether a container has a terminal, which changes how its log stream is framed.
func (c *Client) Tty(ctx context.Context, id string) (bool, error) {
	if !containerID.MatchString(id) {
		return false, fmt.Errorf("docker: %q is not a container id", id)
	}

	response, err := c.get(ctx, "/containers/"+id+"/json", nil)
	if err != nil {
		return false, err
	}

	defer func() { _ = response.Body.Close() }()

	var inspected struct {
		Config struct {
			Tty bool `json:"Tty"`
		} `json:"Config"`
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&inspected); err != nil {
		return false, fmt.Errorf("docker: reading the container: %w", err)
	}

	return inspected.Config.Tty, nil
}

// Logs streams a container's output from `since` on, following it until it stops.
// Every line starts with its time, because of timestamps=1.
func (c *Client) Logs(ctx context.Context, id string, since time.Time) (io.ReadCloser, error) {
	if !containerID.MatchString(id) {
		return nil, fmt.Errorf("docker: %q is not a container id", id)
	}

	query := url.Values{
		"follow":     {"1"},
		"stdout":     {"1"},
		"stderr":     {"1"},
		"timestamps": {"1"},
		"since":      {sinceParameter(since)},
	}

	response, err := c.get(ctx, "/containers/"+id+"/logs", query)
	if err != nil {
		return nil, err
	}

	return response.Body, nil
}

// sinceParameter is the engine's "seconds.nanoseconds" form; the zero time is 0.
func sinceParameter(at time.Time) string {
	if at.IsZero() || at.Unix() < 0 {
		return "0"
	}

	return fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond())
}
