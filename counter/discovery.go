package main

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

// The counter reads ServedModels without importing the oven: it is a separate module and
// needs only these few fields. They match oven/api/v1alpha1/servedmodel_types.go.
const (
	servedModelGroupVersion = "serving.kukie.io/v1alpha1"
	servedModelResource     = "servedmodels"
	servedModelPort         = 8000
	phaseStopped            = "Stopped"

	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	// maxListBytes bounds one list response. A namespace of ServedModels is far smaller.
	maxListBytes = 8 << 20
	// maxServedNameLength matches the oven's pattern for spec.source.huggingface.
	maxServedNameLength = 200
)

// servedModel is what the counter needs from one ServedModel.
type servedModel struct {
	Name       string
	Namespace  string
	Generation int64
	// ServedNames are the "model" values vLLM answers to, the first being its own name.
	ServedNames []string
	Phase       string
	Deleting    bool
	Baseline    *servedModelBaseline
}

type servedModelBaseline struct {
	MaxConcurrency int64 `json:"maxConcurrency"`
	Partial        bool  `json:"partial"`
	Generation     int64 `json:"generation"`
}

// upstream is the Service the oven makes for the model.
func (m servedModel) upstream() *url.URL {
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("%s.%s.svc:%d", m.Name, m.Namespace, servedModelPort)}
}

// servedModelLister lists the ServedModels of the configured namespace.
type servedModelLister interface {
	list(ctx context.Context) ([]servedModel, error)
}

// kubeLister calls the API server with plain HTTP: a list every interval does not need an
// informer, and client-go would bring in dozens of modules for one GET.
type kubeLister struct {
	listURL   string
	client    *http.Client
	tokenPath string
}

// newInClusterLister uses the Pod's service account. The token file is read on every call
// because the kubelet rotates it.
func newInClusterLister(namespace string) (*kubeLister, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("discovery needs to run in a cluster: KUBERNETES_SERVICE_HOST is not set")
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("discovery: read the service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("discovery: the service account CA has no certificate")
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
	}
	base := "https://" + net.JoinHostPort(host, port)
	return newKubeLister(base, namespace, &http.Client{Transport: transport}, serviceAccountDir+"/token"), nil
}

func newKubeLister(base, namespace string, client *http.Client, tokenPath string) *kubeLister {
	return &kubeLister{
		listURL: fmt.Sprintf("%s/apis/%s/namespaces/%s/%s",
			strings.TrimRight(base, "/"), servedModelGroupVersion, url.PathEscape(namespace), servedModelResource),
		client: &http.Client{
			Transport: client.Transport,
			Timeout:   10 * time.Second,
			// The API server does not redirect; following one could send the token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tokenPath: tokenPath,
	}
}

func (k *kubeLister) list(ctx context.Context) ([]servedModel, error) {
	token, err := os.ReadFile(k.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read the service account token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	res, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// The body is not logged: it could repeat anything, and the status says enough.
		return nil, fmt.Errorf("list %s: HTTP %d", servedModelResource, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxListBytes {
		return nil, fmt.Errorf("list %s: response larger than %d bytes", servedModelResource, maxListBytes)
	}
	return parseServedModelList(body)
}

func parseServedModelList(body []byte) ([]servedModel, error) {
	var list struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				Namespace         string  `json:"namespace"`
				Generation        int64   `json:"generation"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Source struct {
					HuggingFace string `json:"huggingface"`
				} `json:"source"`
				Engine struct {
					Args []string `json:"args"`
				} `json:"engine"`
			} `json:"spec"`
			Status struct {
				Phase    string               `json:"phase"`
				Baseline *servedModelBaseline `json:"baseline"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse %s list: %w", servedModelResource, err)
	}
	if list.Metadata.Continue != "" {
		// No limit is sent, so the API server returns everything at once.
		return nil, fmt.Errorf("parse %s list: the response is paginated", servedModelResource)
	}
	models := make([]servedModel, 0, len(list.Items))
	for _, item := range list.Items {
		if !namespacePattern.MatchString(item.Metadata.Namespace) || !namespacePattern.MatchString(item.Metadata.Name) {
			continue // A Service name must be a DNS label, so this could not be routed anyway.
		}
		models = append(models, servedModel{
			Name:        item.Metadata.Name,
			Namespace:   item.Metadata.Namespace,
			Generation:  item.Metadata.Generation,
			ServedNames: servedNames(item.Spec.Source.HuggingFace, item.Spec.Engine.Args),
			Phase:       item.Status.Phase,
			Deleting:    item.Metadata.DeletionTimestamp != nil,
			Baseline:    item.Status.Baseline,
		})
	}
	return models, nil
}

// servedNames is what vLLM accepts as "model": the --served-model-name values if the
// engine args give any, otherwise the --model value, which the oven sets to the Hub id.
func servedNames(huggingFace string, args []string) []string {
	var names []string
	for i := 0; i < len(args); i++ {
		flag, value, hasValue := strings.Cut(args[i], "=")
		if flag != "--served-model-name" && flag != "--served_model_name" {
			continue
		}
		names = nil // argparse keeps the last occurrence.
		if hasValue {
			names = append(names, value)
			continue
		}
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			names = append(names, args[i])
		}
	}
	if len(names) == 0 {
		names = []string{huggingFace}
	}
	valid := names[:0]
	for _, name := range names {
		if validServedName(name) {
			valid = append(valid, name)
		}
	}
	return valid
}

// validServedName keeps metric labels and the model list to names a person could have
// meant: no control characters or spaces, and not the label for unknown models.
func validServedName(name string) bool {
	if name == "" || name == labelUnknown || len(name) > maxServedNameLength {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}
