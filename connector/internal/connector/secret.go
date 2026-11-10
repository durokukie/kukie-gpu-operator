package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
var keyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// SecretWriter updates one key of the connector's own Secret through the
// in-cluster API. It needs only get/update on that Secret by name and uses
// resourceVersion so a concurrent edit is never overwritten blindly.
type SecretWriter struct {
	base, namespace, name, key, tokenFile string
	http                                  *http.Client
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewInClusterSecretWriter uses the Pod's mounted service account.
func NewInClusterSecretWriter(name, key string) (*SecretWriter, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	ns, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil || host == "" || port == "" {
		return nil, errors.New("not running in a cluster with a mounted service account token")
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, errors.New("missing service account CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid service account CA")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}
	return NewSecretWriter("https://"+net.JoinHostPort(host, port), strings.TrimSpace(string(ns)), name, key, serviceAccountDir+"/token", client)
}

func NewSecretWriter(base, namespace, name, key, tokenFile string, client *http.Client) (*SecretWriter, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || !namePattern.MatchString(namespace) || !namePattern.MatchString(name) || !keyPattern.MatchString(key) || client == nil {
		return nil, errors.New("invalid Secret configuration")
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &SecretWriter{base: u.Scheme + "://" + u.Host, namespace: namespace, name: name, key: key, tokenFile: tokenFile, http: &copied}, nil
}

// Write stores token under the key. Errors carry only HTTP status codes.
func (w *SecretWriter) Write(ctx context.Context, token string) error {
	path := w.base + "/api/v1/namespaces/" + w.namespace + "/secrets/" + w.name
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	for range 3 {
		var secret map[string]any
		if err := w.do(ctx, http.MethodGet, path, nil, &secret); err != nil {
			return err
		}
		data, _ := secret["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
			secret["data"] = data
		}
		if data[w.key] == encoded {
			return nil
		}
		data[w.key] = encoded
		err := w.do(ctx, http.MethodPut, path, secret, nil)
		var status statusError
		if errors.As(err, &status) && status == http.StatusConflict {
			continue
		}
		return err
	}
	return errors.New("Secret kept changing; update conflicted 3 times")
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("kubernetes API status %d", int(e)) }

func (w *SecretWriter) do(ctx context.Context, method, path string, in any, out any) error {
	bearer, err := os.ReadFile(w.tokenFile)
	if err != nil {
		return errors.New("cannot read service account token")
	}
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return errors.New("cannot encode Secret")
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		return errors.New("invalid kubernetes request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(bearer)))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := w.http.Do(req)
	if err != nil {
		return errors.New("kubernetes API unreachable")
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return statusError(response.StatusCode)
	}
	if err != nil || len(b) > 1<<20 {
		return errors.New("kubernetes API response too large")
	}
	if out != nil && json.Unmarshal(b, out) != nil {
		return errors.New("invalid kubernetes API response")
	}
	return nil
}
