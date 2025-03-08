/*
 * Warp (C) 2019-2020 MinIO, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package cli

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/cli"
	"github.com/minio/madmin-go/v3"
	"github.com/minio/mc/pkg/probe"
	md5simd "github.com/minio/md5-simd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/pkg/v3/certs"
	"github.com/minio/pkg/v3/console"
	"github.com/minio/pkg/v3/ellipses"
	"github.com/minio/warp/pkg"
	"golang.org/x/net/http2"
)

type hostSelectType string

const (
	hostSelectTypeRoundrobin hostSelectType = "roundrobin"
	hostSelectTypeWeighed    hostSelectType = "weighed"
)

func newClient(ctx *cli.Context) func() (cl *minio.Client, done func()) {
	hosts := parseHosts(ctx.String("host"), ctx.Bool("resolve-host"))
	switch len(hosts) {
	case 0:
		fatalIf(probe.NewError(errors.New("no host defined")), "Unable to create MinIO client")
	case 1:
		cl, err := getClient(ctx, hosts[0])
		fatalIf(probe.NewError(err), "Unable to create MinIO client")

		return func() (*minio.Client, func()) {
			return cl, func() {}
		}
	}
	hostSelect := hostSelectType(ctx.String("host-select"))
	switch hostSelect {
	case hostSelectTypeRoundrobin:
		// Do round-robin.
		var current int
		var mu sync.Mutex
		clients := make([]*minio.Client, len(hosts))
		for i := range hosts {
			cl, err := getClient(ctx, hosts[i])
			fatalIf(probe.NewError(err), "Unable to create MinIO client")
			clients[i] = cl
		}
		return func() (*minio.Client, func()) {
			mu.Lock()
			now := current % len(clients)
			current++
			mu.Unlock()
			return clients[now], func() {}
		}
	case hostSelectTypeWeighed:
		// Keep track of handed out clients.
		// Select random between the clients that have the fewest handed out.
		var mu sync.Mutex
		clients := make([]*minio.Client, len(hosts))
		for i := range hosts {
			cl, err := getClient(ctx, hosts[i])
			fatalIf(probe.NewError(err), "Unable to create MinIO client")
			clients[i] = cl
		}
		running := make([]int, len(hosts))
		lastFinished := make([]time.Time, len(hosts))
		{
			// Start with a random host
			now := time.Now()
			off := rand.New(rand.NewSource(time.Now().UnixNano())).Intn(len(hosts))
			for i := range lastFinished {
				lastFinished[i] = now.Add(time.Duration(i + off%len(hosts)))
			}
		}
		find := func() int {
			minSize := math.MaxInt32
			for _, n := range running {
				if n < minSize {
					minSize = n
				}
			}
			earliest := time.Now().Add(time.Second)
			earliestIdx := 0
			for i, n := range running {
				if n == minSize {
					if lastFinished[i].Before(earliest) {
						earliest = lastFinished[i]
						earliestIdx = i
					}
				}
			}
			return earliestIdx
		}
		return func() (*minio.Client, func()) {
			mu.Lock()
			idx := find()
			running[idx]++
			mu.Unlock()
			return clients[idx], func() {
				mu.Lock()
				lastFinished[idx] = time.Now()
				running[idx]--
				if running[idx] < 0 {
					// Will happen if done is called twice.
					panic("client running index < 0")
				}
				mu.Unlock()
			}
		}
	}
	console.Fatalln("unknown host-select:", hostSelect)
	return nil
}

// getClient creates a client with the specified host and the options set in the context.
func getClient(ctx *cli.Context, host string) (*minio.Client, error) {
	var creds *credentials.Credentials
	switch strings.ToUpper(ctx.String("signature")) {
	case "S3V4":
		// if Signature version '4' use NewV4 directly.
		creds = credentials.NewStaticV4(ctx.String("access-key"), ctx.String("secret-key"), "")
	case "S3V2":
		// if Signature version '2' use NewV2 directly.
		creds = credentials.NewStaticV2(ctx.String("access-key"), ctx.String("secret-key"), "")
	default:
		fatal(probe.NewError(errors.New("unknown signature method. S3V2 and S3V4 is available")), strings.ToUpper(ctx.String("signature")))
	}
	lookup := minio.BucketLookupAuto
	if ctx.String("lookup") == "host" {
		lookup = minio.BucketLookupDNS
	} else if ctx.String("lookup") == "path" {
		lookup = minio.BucketLookupPath
	}
	cl, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       ctx.Bool("tls"),
		Region:       ctx.String("region"),
		BucketLookup: lookup,
		CustomMD5:    md5simd.NewServer().NewHash,
		Transport:    clientTransport(ctx),
	})
	if err != nil {
		return nil, err
	}
	cl.SetAppInfo(appName, pkg.Version)

	if ctx.Bool("debug") {
		cl.TraceOn(os.Stderr)
	}

	return cl, nil
}

type CustomTransport struct {
	Transport http.RoundTripper
	Headers   map[string]string
	Url *url.URL
}

func (c *CustomTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Add custom headers to the request
	for key, value := range c.Headers {
		req.Header.Add(key, value)
	}

	if c.Url != nil {
		req.URL.Host = c.Url.Host
		req.URL.Scheme = c.Url.Scheme
		req.Host = c.Url.Host
	}

	// Proceed with the original request using the custom headers
	return c.Transport.RoundTrip(req)
}

func getAdditionalHttpHeaders(ctx *cli.Context) map[string]string {
	headers := make(map[string]string)
	for _, flag := range []string{"http-header"} {
		for _, v := range ctx.StringSlice(flag) {
			idx := strings.Index(v, "=")
			if idx <= 0 {
				console.Fatalf("--%s takes `key=value` argument", flag)
			}
			key := v[:idx]
			value := v[idx+1:]
			if len(value) == 0 {
				console.Fatal("--%s value can't be empty", flag)
			}
			headers[key] = value
		}
	}

	return headers
}

func clientTransport(ctx *cli.Context) http.RoundTripper {

	headers := getAdditionalHttpHeaders(ctx)

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   ctx.Int("concurrent"),
		WriteBufferSize:       ctx.Int("sndbuf"), // Configure beyond 4KiB default buffer size.
		ReadBufferSize:        ctx.Int("rcvbuf"), // Configure beyond 4KiB default buffer size.
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		// Set this value so that the underlying transport round-tripper
		// doesn't try to auto decode the body of objects with
		// content-encoding set to `gzip`.
		//
		// Refer:
		//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
		DisableCompression: true,
		DisableKeepAlives:  ctx.Bool("disable-http-keepalive"),
	}
	if ctx.Bool("tls") {
		// Keep TLS config.
		tr.TLSClientConfig = &tls.Config{
			RootCAs: mustGetSystemCertPool(),
			// Can't use SSLv3 because of POODLE and BEAST
			// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
			// Can't use TLSv1.1 because of RC4 cipher usage
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: ctx.Bool("insecure"),
			ClientSessionCache: tls.NewLRUClientSessionCache(1024), // up to 1024 nodes
		}

		// Because we create a custom TLSClientConfig, we have to opt-in to HTTP/2.
		// See https://github.com/golang/go/issues/14275
		if ctx.Bool("http2") {
			http2.ConfigureTransport(tr)
		}
	}

	ctr := 	&CustomTransport{
			Transport : tr,
			Headers : headers}  // Inject custom headers here

	host_url := ctx.String("proxy-host")
	if host_url != "" {
		u, err := url.Parse(host_url)
		if err != nil {
			fatalIf(probe.NewError(err), "Unable to parse proxy host URL")
		}
		ctr.Url = u
	}

	return ctr
}

// parseHosts will parse the host parameter given.
func parseHosts(h string, resolveDNS bool) []string {
	hosts := strings.Split(h, ",")
	var dst []string
	for _, host := range hosts {
		if !ellipses.HasEllipses(host) {
			if !strings.HasPrefix(host, "file:") {
				dst = append(dst, host)
				continue
			}
			// If host starts with file:, then it is a file containing hosts.
			f, err := os.Open(strings.TrimPrefix(host, "file:"))
			if err != nil {
				fatalIf(probe.NewError(err), "Unable to open host file")
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				host := strings.TrimSpace(scanner.Text())
				if len(host) == 0 {
					continue
				}
				if !ellipses.HasEllipses(host) {
					dst = append(dst, host)
					continue
				}
				patterns, perr := ellipses.FindEllipsesPatterns(host)
				if perr != nil {
					fatalIf(probe.NewError(perr), fmt.Sprintf("Unable to parse host parameter: %s", host))
				}
				for _, lbls := range patterns.Expand() {
					dst = append(dst, strings.Join(lbls, ""))
				}
			}
			if err := scanner.Err(); err != nil {
				fatalIf(probe.NewError(err), "Unable to read host file")
			}
			continue
		}
		patterns, perr := ellipses.FindEllipsesPatterns(host)
		if perr != nil {
			fatalIf(probe.NewError(perr), "Unable to parse host parameter")
		}
		for _, lbls := range patterns.Expand() {
			dst = append(dst, strings.Join(lbls, ""))
		}
	}

	if !resolveDNS {
		return dst
	}

	var resolved []string
	for _, hostport := range dst {
		host, port, _ := net.SplitHostPort(hostport)
		if host == "" {
			host = hostport
		}
		ips, err := net.LookupIP(host)
		if err != nil {
			fatalIf(probe.NewError(err), "Could not get IPs for "+hostport)
		}
		for _, ip := range ips {
			if port == "" {
				resolved = append(resolved, ip.String())
			} else {
				resolved = append(resolved, ip.String()+":"+port)
			}
		}
	}
	return resolved
}

// mustGetSystemCertPool - return system CAs or empty pool in case of error (or windows)
func mustGetSystemCertPool() *x509.CertPool {
	rootCAs, err := certs.GetRootCAs("")
	if err != nil {
		rootCAs, err = x509.SystemCertPool()
		if err != nil {
			return x509.NewCertPool()
		}
	}
	return rootCAs
}

func newAdminClient(ctx *cli.Context) *madmin.AdminClient {
	hosts := parseHosts(ctx.String("host"), ctx.Bool("resolve-host"))
	if len(hosts) == 0 {
		fatalIf(probe.NewError(errors.New("no host defined")), "Unable to create MinIO admin client")
	}

	cl, err := madmin.NewWithOptions(hosts[0], &madmin.Options{
		Creds:     credentials.NewStaticV4(ctx.String("access-key"), ctx.String("secret-key"), ""),
		Secure:    ctx.Bool("tls"),
		Transport: clientTransport(ctx),
	})
	fatalIf(probe.NewError(err), "Unable to create MinIO admin client")
	cl.SetAppInfo(appName, pkg.Version)
	return cl
}
