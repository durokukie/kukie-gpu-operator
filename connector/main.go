// Command connector maintains outbound authenticated sessions to Kukie.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/durokukie/kukie-gpu-operator/connector/internal/connector"
)

func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "connector failed: check configuration and mounted files")
		os.Exit(1)
	}
}
func run() error {
	gateway := flag.String("gateway", "", "HTTPS gateway /connector/v1/connect URL")
	cluster := flag.String("cluster-id", "", "registered cluster ID")
	credential := flag.String("credential-file", "", "mounted connector credential")
	counter := flag.String("counter-url", "", "internal counter /v1 base URL")
	counterCredential := flag.String("counter-credential-file", "", "mounted counter service key")
	caFile := flag.String("ca-file", "", "optional additional gateway CA bundle")
	workers := flag.Int("workers", 4, "outbound connections (1-8)")
	secret := flag.String("credential-secret", "", "this connector's own Secret (same namespace) to save renewed credentials; empty disables renewal")
	secretKey := flag.String("credential-secret-key", "credential", "key of the credential in --credential-secret")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if *caFile != "" {
		b, err := os.ReadFile(*caFile)
		if err != nil {
			return err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return fmt.Errorf("invalid CA bundle")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	read := func(path string) func() (string, error) {
		return func() (string, error) {
			f, err := os.Open(path)
			if err != nil {
				return "", err
			}
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, 4097))
			if err != nil || len(b) > 4096 {
				return "", fmt.Errorf("invalid credential file")
			}
			return strings.TrimSpace(string(b)), nil
		}
	}
	var persist func(context.Context, string) error
	if *secret != "" {
		writer, err := connector.NewInClusterSecretWriter(*secret, *secretKey)
		if err != nil {
			return err
		}
		persist = writer.Write
	}
	c, err := connector.New(connector.Config{GatewayURL: *gateway, ClusterID: *cluster, CounterURL: *counter, Workers: *workers, Credential: read(*credential), CounterCredential: read(*counterCredential), GatewayClient: &http.Client{Transport: transport}, Persist: persist, Logger: logger})
	if err != nil {
		return err
	}
	if _, err := read(*credential)(); err != nil {
		return err
	}
	if _, err := read(*counterCredential)(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("connector starting", "workers", *workers, "autoRenew", persist != nil)
	c.Run(ctx)
	logger.Info("connector stopped")
	return nil
}
