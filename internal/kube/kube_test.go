package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const uid = "0a1b2c3d-0000-0000-0000-000000000001"

func TestParsePodLog(t *testing.T) {
	root := "/var/log/pods"

	for name, tc := range map[string]struct {
		path string
		want PodLog
		ok   bool
	}{
		"a container log":    {root + "/shop_cart-7d9f-abc12_" + uid + "/app/0.log", PodLog{"shop", "cart-7d9f-abc12", uid, "app", 0}, true},
		"after restarts":     {root + "/shop_cart_" + uid + "/app/12.log", PodLog{"shop", "cart", uid, "app", 12}, true},
		"a rotated file":     {root + "/shop_cart_" + uid + "/app/0.log.20261027-101500", PodLog{}, false},
		"a compressed one":   {root + "/shop_cart_" + uid + "/app/0.log.20261027-101500.gz", PodLog{}, false},
		"not a number":       {root + "/shop_cart_" + uid + "/app/latest.log", PodLog{}, false},
		"a leading zero":     {root + "/shop_cart_" + uid + "/app/01.log", PodLog{}, false},
		"too shallow":        {root + "/shop_cart_" + uid + "/0.log", PodLog{}, false},
		"too deep":           {root + "/shop_cart_" + uid + "/app/x/0.log", PodLog{}, false},
		"a bad owner":        {root + "/shop_cart/app/0.log", PodLog{}, false},
		"too many underline": {root + "/shop_cart_" + uid + "_extra/app/0.log", PodLog{}, false},
		"outside the dir":    {"/etc/passwd", PodLog{}, false},
		"above the dir":      {root + "/../shop_cart_" + uid + "/app/0.log", PodLog{}, false},
		"an empty part":      {root + "/_cart_" + uid + "/app/0.log", PodLog{}, false},
	} {
		got, ok := ParsePodLog(root, tc.path)

		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %+v %v", name, got, ok)
		}
	}
}

func TestParseContainerLink(t *testing.T) {
	id := strings.Repeat("ab12", 16)

	got, ok := ParseContainerLink("cart-7d9f-abc12_shop_app-" + id + ".log")
	if !ok || got != (ContainerLink{"cart-7d9f-abc12", "shop", "app", id}) {
		t.Errorf("got %+v %v", got, ok)
	}

	for _, bad := range []string{"cart_shop_app.log", "cart_shop_app-" + id[:10] + ".log", "cart_shop-" + id + ".log", "cart_shop_app-" + id, "a_b_c_d-" + id + ".log"} {
		if _, ok := ParseContainerLink(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}

	if _, ok := ParseContainerLink("cart_shop_app-" + strings.ToUpper(id) + ".log"); ok {
		t.Error("a container id is lower case hex")
	}
}

func TestWorkloadName(t *testing.T) {
	for _, tc := range []struct{ kind, name, want string }{
		{"ReplicaSet", "cart-7d9f8b6c5d", "cart"},
		{"ReplicaSet", "cart-api-7d9f8b6c5d", "cart-api"},
		{"ReplicaSet", "standalone", "standalone"},
		{"StatefulSet", "db", "db"},
		{"DaemonSet", "node-exporter", "node-exporter"},
		{"Job", "nightly-29012345", "nightly"},
		{"Job", "migrate", "migrate"},
		{"Job", "retry-3", "retry-3"},
	} {
		if got := WorkloadName(tc.kind, tc.name); got != tc.want {
			t.Errorf("%s %s: got %s", tc.kind, tc.name, got)
		}
	}
}

func TestMapperAcceptsByNamespacePodAndContainer(t *testing.T) {
	m := &Mapper{ExcludeNamespaces: []string{"kube-*"}, ExcludePods: []string{"tailcove-agent-*"}, ExcludeContainers: []string{"istio-proxy"}}

	for _, tc := range []struct {
		ns, pod, container string
		want               bool
	}{
		{"shop", "cart-1", "app", true},
		{"kube-system", "coredns-1", "coredns", false},
		{"monitoring", "tailcove-agent-x1", "agent", false},
		{"shop", "cart-1", "istio-proxy", false},
	} {
		if got := m.Accept(tc.ns, tc.pod, tc.container); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}

	included := &Mapper{IncludeNamespaces: []string{"shop", "pay*"}, ExcludeNamespaces: []string{"payments-test"}}

	for ns, want := range map[string]bool{"shop": true, "payments": true, "payments-test": false, "other": false} {
		if got := included.Accept(ns, "p", "c"); got != want {
			t.Errorf("include list, %s: got %v", ns, got)
		}
	}
}

func TestMapperNamesTheHost(t *testing.T) {
	meta := Meta{Pod: "cart-1", Node: "node-a", Workload: "cart", Labels: map[string]string{"app": "checkout"}}

	for host, want := range map[string]string{"": "cart-1", "pod": "cart-1", "node": "node-a", "workload": "cart", "label:app": "checkout", "label:missing": "cart-1"} {
		if got := (&Mapper{Host: host}).HostFor(meta); got != want {
			t.Errorf("host %q: got %q want %q", host, got, want)
		}
	}

	// What is not known falls back to the pod, never to an empty host.
	if got := (&Mapper{Host: "workload"}).HostFor(Meta{Pod: "p"}); got != "p" {
		t.Errorf("got %q", got)
	}
}

func TestMapperRoutesToTheFirstMatch(t *testing.T) {
	m := &Mapper{Routes: []Route{
		{Namespace: "payments*", Token: "tcs_payments"},
		{Labels: map[string]string{"tier": "gold", "team": "a"}, Token: "tcs_gold"},
		{Namespace: "shop", Labels: map[string]string{"app": "cart"}, Token: "tcs_cart"},
	}}

	for name, tc := range map[string]struct {
		meta Meta
		want string
	}{
		"by namespace glob": {Meta{Namespace: "payments-eu"}, "tcs_payments"},
		"by both labels":    {Meta{Namespace: "x", Labels: map[string]string{"tier": "gold", "team": "a"}}, "tcs_gold"},
		"one label of two":  {Meta{Namespace: "x", Labels: map[string]string{"tier": "gold"}}, ""},
		"namespace and app": {Meta{Namespace: "shop", Labels: map[string]string{"app": "cart"}}, "tcs_cart"},
		"app but not ns":    {Meta{Namespace: "other", Labels: map[string]string{"app": "cart"}}, ""},
		"no labels known":   {Meta{Namespace: "shop"}, ""},
		"nothing matches":   {Meta{Namespace: "misc"}, ""},
	} {
		if got := m.TokenFor(tc.meta); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestMapperKeepsOnlyTheLabelsItWasAskedForAndBoundsThem(t *testing.T) {
	long := strings.Repeat("é", 200)

	m := &Mapper{LabelKeys: []string{"app", "version", "missing"}}
	kept := m.KeptLabels(map[string]string{"app": "cart", "version": long, "secret": "no", "extra": "x"})

	if len(kept) != 2 || kept["app"] != "cart" || kept["secret"] != "" {
		t.Errorf("kept %v", kept)
	}

	if len(kept["version"]) > MaxLabelValue || !strings.HasPrefix(long, kept["version"]) {
		t.Errorf("a value is cut to %d bytes on a character boundary, got %d", MaxLabelValue, len(kept["version"]))
	}

	many := &Mapper{}
	all := map[string]string{}

	for i := range 30 {
		key := string(rune('a' + i%26))
		many.LabelKeys = append(many.LabelKeys, key+string(rune('0'+i/26)))
		all[key+string(rune('0'+i/26))] = "v"
	}

	if got := many.KeptLabels(all); len(got) != MaxLabels {
		t.Errorf("kept %d labels, the limit is %d", len(got), MaxLabels)
	}
}

func TestMapperContext(t *testing.T) {
	m := &Mapper{LabelKeys: []string{"app"}}
	got := m.Context(Meta{Namespace: "shop", Pod: "cart-1", Container: "app", Restarts: 2, Node: "n1", Workload: "cart", Labels: map[string]string{"app": "cart", "x": "y"}})

	encoded, _ := json.Marshal(got)

	if string(encoded) != `{"container":"app","labels":{"app":"cart"},"namespace":"shop","node":"n1","pod":"cart-1","restarts":2,"workload":"cart"}` {
		t.Errorf("got %s", encoded)
	}

	bare, _ := json.Marshal(m.Context(Meta{Namespace: "shop", Pod: "p", Container: "c"}))

	if string(bare) != `{"container":"c","namespace":"shop","pod":"p","restarts":0}` {
		t.Errorf("got %s", bare)
	}
}

// --- the API -------------------------------------------------------------------------------------------------

func fakeAPI(t *testing.T, handler http.HandlerFunc) *API {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return &API{URL: server.URL, TokenFile: token, Client: server.Client()}
}

const onePod = `{"items":[{"metadata":{"name":"cart-7d9f8b6c5d-abc12","namespace":"shop","uid":"` + uid + `","labels":{"app":"cart","tier":"web"},
"ownerReferences":[{"kind":"ReplicaSet","name":"cart-7d9f8b6c5d","controller":true}]},
"status":{"containerStatuses":[{"name":"app","containerID":"containerd://` + "abcd" + `","restartCount":2}],"initContainerStatuses":[{"name":"init","containerID":"containerd://ef01","restartCount":0}]}}]}`

func TestTheAPIListsThePodsOfOneNodeReadOnly(t *testing.T) {
	var path, selector, auth, method string

	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		path, selector, auth, method = r.URL.Path, r.URL.Query().Get("fieldSelector"), r.Header.Get("Authorization"), r.Method
		_, _ = w.Write([]byte(onePod))
	})

	pods, err := api.PodsOnNode(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}

	if method != http.MethodGet || path != "/api/v1/pods" || selector != "spec.nodeName=node-a" || auth != "Bearer sa-token" {
		t.Errorf("asked %s %s %q with %q", method, path, selector, auth)
	}

	if len(pods) != 1 {
		t.Fatalf("got %d pods", len(pods))
	}

	pod := pods[0]

	if pod.Namespace != "shop" || pod.UID != uid || pod.Workload != "cart" || pod.Labels["tier"] != "web" {
		t.Errorf("got %+v", pod)
	}

	if pod.Containers["app"] != (ContainerInfo{ID: "abcd", Restarts: 2}) || pod.Containers["init"].ID != "ef01" {
		t.Errorf("containers %+v", pod.Containers)
	}
}

func TestTheAPIFollowsPagesAndStopsAtALimit(t *testing.T) {
	var calls atomic.Int32

	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"metadata":{"continue":"more"},"items":[]}`))
	})

	if _, err := api.PodsOnNode(context.Background(), "n"); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != maxPodPages {
		t.Errorf("asked %d times, the limit is %d", calls.Load(), maxPodPages)
	}
}

func TestTheAPIExplainsARefusalWithoutQuotingAnything(t *testing.T) {
	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"pods is forbidden: User \"system:serviceaccount:x:y\" cannot list"}`))
	})

	_, err := api.PodsOnNode(context.Background(), "n")

	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "system:serviceaccount") {
		t.Errorf("got %v", err)
	}
}

func TestTheAPIRejectsAnOversizedListAndABadToken(t *testing.T) {
	big := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[` + strings.Repeat(" ", maxPodListBytes+10) + `]}`))
	})

	if _, err := big.PodsOnNode(context.Background(), "n"); err == nil {
		t.Error("an oversized list was accepted")
	}

	missing := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	missing.TokenFile = filepath.Join(t.TempDir(), "nope")

	if _, err := missing.PodsOnNode(context.Background(), "n"); err == nil {
		t.Error("a missing token file was accepted")
	}
}

func TestInClusterNeedsAPod(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	if _, err := InCluster(); err == nil {
		t.Error("InCluster worked outside a pod")
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	api, err := InCluster()
	if err != nil || api.URL != "https://10.43.0.1:443" || !strings.HasPrefix(api.TokenFile, ServiceAccountDir) {
		t.Errorf("got %+v %v", api, err)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")

	if api, _ := InCluster(); api.URL != "https://[fd00::1]:443" {
		t.Errorf("an IPv6 address needs brackets: %s", api.URL)
	}
}

// --- the registry --------------------------------------------------------------------------------------------

func registry(t *testing.T, api *API, now *time.Time) (*Registry, *[]string) {
	t.Helper()

	var logs []string

	return &Registry{
		PodsDir: "/var/log/pods", Node: "node-a", API: api, Mapper: &Mapper{},
		Interval: 30 * time.Second, MinRefresh: 2 * time.Second,
		Logf: func(format string, args ...any) { logs = append(logs, format) },
		Now:  func() time.Time { return *now },
	}, &logs
}

var podPath = "/var/log/pods/shop_cart-7d9f8b6c5d-abc12_" + uid + "/app/0.log"

func TestLookupWithoutTheAPIUsesThePath(t *testing.T) {
	now := time.Now()
	r, _ := registry(t, nil, &now)

	meta, ok := r.Lookup(context.Background(), podPath)

	if !ok || meta.Namespace != "shop" || meta.Pod != "cart-7d9f8b6c5d-abc12" || meta.Container != "app" || meta.Node != "node-a" || meta.Enriched {
		t.Errorf("got %+v %v", meta, ok)
	}

	if _, ok := r.Lookup(context.Background(), "/var/log/syslog"); ok {
		t.Error("a file that is not a pod log was taken for one")
	}
}

func TestLookupAddsWhatTheAPIKnowsAndAsksAgainOnlyWhenItMust(t *testing.T) {
	var calls atomic.Int32

	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(onePod))
	})

	now := time.Now()
	r, _ := registry(t, api, &now)

	meta, _ := r.Lookup(context.Background(), podPath)

	if !meta.Enriched || meta.Workload != "cart" || meta.Labels["app"] != "cart" {
		t.Fatalf("got %+v", meta)
	}

	for range 50 {
		_, _ = r.Lookup(context.Background(), podPath)
	}

	if calls.Load() != 1 {
		t.Errorf("a known pod made %d requests", calls.Load())
	}

	// A pod that is not listed (yet) makes it ask again, but not more than once in MinRefresh.
	unknown := "/var/log/pods/shop_new-1_0a1b2c3d-0000-0000-0000-000000000099/app/0.log"

	for range 20 {
		if meta, _ := r.Lookup(context.Background(), unknown); meta.Enriched {
			t.Fatal("an unlisted pod was enriched")
		}
	}

	if calls.Load() != 1 {
		t.Errorf("an unknown pod asked at once, %d requests", calls.Load())
	}

	now = now.Add(3 * time.Second)
	_, _ = r.Lookup(context.Background(), unknown)
	_, _ = r.Lookup(context.Background(), unknown)

	if calls.Load() != 2 {
		t.Errorf("after the minimum wait it should ask once more, %d requests", calls.Load())
	}

	now = now.Add(time.Minute)
	_, _ = r.Lookup(context.Background(), podPath)

	if calls.Load() != 3 {
		t.Errorf("an old list is read again, %d requests", calls.Load())
	}
}

func TestAnAPIFailureIsSaidOnceAndLinesStillGetTheirPath(t *testing.T) {
	var healthy atomic.Bool

	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(onePod))
	})

	now := time.Now()
	r, logs := registry(t, api, &now)

	for range 5 {
		now = now.Add(5 * time.Second)

		meta, ok := r.Lookup(context.Background(), podPath)
		if !ok || meta.Enriched || meta.Pod == "" {
			t.Fatalf("got %+v %v", meta, ok)
		}
	}

	if len(*logs) != 1 {
		t.Errorf("an outage was reported %d times", len(*logs))
	}

	healthy.Store(true)
	now = now.Add(time.Minute)

	if meta, _ := r.Lookup(context.Background(), podPath); !meta.Enriched {
		t.Error("the registry did not recover")
	}

	if len(*logs) != 2 {
		t.Errorf("the recovery should be said once, logs: %v", *logs)
	}
}

func TestWorkloadOfAPodByName(t *testing.T) {
	api := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(onePod)) })
	now := time.Now()
	r, _ := registry(t, api, &now)

	if got := r.WorkloadOf(context.Background(), "shop", "cart-7d9f8b6c5d-abc12"); got != "cart" {
		t.Errorf("got %q", got)
	}

	if got := r.WorkloadOf(context.Background(), "shop", "other"); got != "" {
		t.Errorf("got %q", got)
	}

	if got := (&Registry{}).WorkloadOf(context.Background(), "shop", "x"); got != "" {
		t.Errorf("no API: got %q", got)
	}
}

func TestListRunningReadsTheContainerLinks(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("0f", 32)

	for _, name := range []string{"cart-1_shop_app-" + id + ".log", "notes.txt", "bad_name.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := ListRunning(dir)

	if len(got) != 1 || got[0] != (Running{"shop", "cart-1", "app", id}) {
		t.Errorf("got %+v", got)
	}

	if ListRunning(filepath.Join(dir, "missing")) != nil {
		t.Error("a missing directory is not an error, just nothing")
	}
}
