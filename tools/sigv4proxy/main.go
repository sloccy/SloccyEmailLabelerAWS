// Command sigv4proxy is a tiny local reverse proxy that signs each request with
// AWS SigV4 and forwards it to the IAM-protected Web UI Function URL, so you can
// browse the management UI in a normal browser.
//
// Usage (after authenticating your AWS CLI so credentials are available):
//
//	go run ./tools/sigv4proxy
//	# then open http://localhost:8080
//
// Flags let you override the target URL, region, and listen address.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "local address to listen on")
	target := flag.String("target",
		"https://q4dtacep2n7g2kzhuirm2pexry0amfcy.lambda-url.us-east-1.on.aws",
		"Web UI Function URL to sign requests to")
	region := flag.String("region", "us-east-1", "AWS region of the function")
	flag.Parse()

	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("bad --target: %v", err)
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(*region))
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}
	if _, err := cfg.Credentials.Retrieve(context.Background()); err != nil {
		log.Fatalf("no AWS credentials (authenticate your AWS CLI first): %v", err)
	}
	signer := v4.NewSigner()

	proxy := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadGateway)
			return
		}
		//nolint:gosec // G704: forwarding to the fixed --target host is the proxy's whole job
		out, err := http.NewRequestWithContext(r.Context(), r.Method,
			*target+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build request: "+err.Error(), http.StatusBadGateway)
			return
		}
		out.Host = targetURL.Host
		if ct := r.Header.Get("Content-Type"); ct != "" {
			out.Header.Set("Content-Type", ct)
		}

		sum := sha256.Sum256(body)
		creds, err := cfg.Credentials.Retrieve(r.Context())
		if err != nil {
			http.Error(w, "aws credentials: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := signer.SignHTTP(r.Context(), creds, out,
			hex.EncodeToString(sum[:]), "lambda", *region, time.Now()); err != nil {
			http.Error(w, "sign: "+err.Error(), http.StatusInternalServerError)
			return
		}

		resp, err := http.DefaultClient.Do(out) //nolint:gosec // G704: forwards signed request to the fixed --target
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}

	log.Printf("SigV4 proxy listening on http://%s  ->  %s", *listen, *target)
	// ReadHeaderTimeout bounds how long a stalled client can hold a connection open
	// before sending headers. Local dev tool or not, it costs one field to not be
	// trivially tied up by a half-open connection.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           http.HandlerFunc(proxy),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
